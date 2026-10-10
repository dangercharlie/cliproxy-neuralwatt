package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Window is one usage window (session, weekly, billing period) normalised
// across providers. Clients derive pace from resets_at and window_seconds.
type Window struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	UsedPercent   float64 `json:"used_percent"`
	ResetsAt      string  `json:"resets_at,omitempty"`
	WindowSeconds int64   `json:"window_seconds,omitempty"`
	Scope         string  `json:"scope,omitempty"`
	Severity      string  `json:"severity,omitempty"`
}

// Account is one credential's quota view.
type Account struct {
	Provider     string                  `json:"provider"`
	AuthIndex    string                  `json:"auth_index"`
	Label        string                  `json:"label"`
	Email        string                  `json:"email,omitempty"`
	Organization string                  `json:"organization,omitempty"`
	Plan         string                  `json:"plan,omitempty"`
	Disabled     bool                    `json:"disabled,omitempty"`
	Unavailable  bool                    `json:"unavailable,omitempty"`
	Windows      []Window                `json:"windows"`
	Metrics      []pluginapi.QuotaMetric `json:"metrics"`
	Error        string                  `json:"error,omitempty"`
	FetchedAt    string                  `json:"fetched_at,omitempty"`
	// Stale marks a reading served from the last good fetch because the
	// latest attempt failed (StaleReason says why, typically a 429).
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`

	rateLimited bool
}

// Summary is the payload served at GET /v0/management/plugins/neuralwatt/summary.
type Summary struct {
	Plugin      string    `json:"plugin"`
	Version     string    `json:"version"`
	GeneratedAt string    `json:"generated_at"`
	Accounts    []Account `json:"accounts"`
	Error       string    `json:"error,omitempty"`
}

type summaryCache struct {
	mu      sync.Mutex
	entries map[string]cachedAccount
}

// cachedAccount keeps the last result (served while fresh) and the last good
// result (served, flagged stale, when a newer attempt fails).
type cachedAccount struct {
	last   Account
	lastAt time.Time
	good   *Account
	goodAt time.Time
	// backoffUntil blocks upstream fetches after a 429; doubles per failure.
	backoffUntil time.Time
	failures     int
}

const (
	summaryMinRefresh = 20 * time.Second // even ?refresh=1 waits this long per account
	summaryStaleMax   = 15 * time.Minute // serve a stale good reading this long
	backoffInitial    = 2 * time.Minute
	backoffMax        = 15 * time.Minute
)

// persistedCache is the on-disk shape: last good reading per credential.
type persistedCache struct {
	SavedAt  string                      `json:"saved_at"`
	Accounts map[string]persistedAccount `json:"accounts"`
}

type persistedAccount struct {
	Account Account   `json:"account"`
	At      time.Time `json:"at"`
}

func newSummaryCache() *summaryCache { return &summaryCache{entries: map[string]cachedAccount{}} }

func (c *summaryCache) reset() {
	c.mu.Lock()
	c.entries = map[string]cachedAccount{}
	c.mu.Unlock()
}

// get returns the last result when it is younger than ttl, or while the
// credential is backing off after a rate limit (the stale/err result stands).
func (c *summaryCache) get(key string, ttl time.Duration, now time.Time) (Account, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.lastAt.IsZero() {
		return Account{}, false
	}
	if now.Before(e.backoffUntil) || now.Sub(e.lastAt) <= ttl {
		return e.last, true
	}
	return Account{}, false
}

// record stores a fresh result. A failed result is replaced by the last good
// one (flagged stale) when that is recent enough, so a rate-limited upstream
// never blanks a display that was fine a minute ago.
func (c *summaryCache) record(key string, acct Account, now time.Time) Account {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if acct.Error == "" {
		good := acct
		e.good, e.goodAt = &good, now
		e.failures, e.backoffUntil = 0, time.Time{}
	} else {
		if acct.rateLimited {
			e.failures++
			wait := backoffInitial << (e.failures - 1)
			if wait > backoffMax || wait <= 0 {
				wait = backoffMax
			}
			e.backoffUntil = now.Add(wait)
		}
		if e.good != nil && now.Sub(e.goodAt) < summaryStaleMax {
			stale := *e.good
			stale.Stale, stale.StaleReason = true, acct.Error
			acct = stale
		}
	}
	e.last, e.lastAt = acct, now
	c.entries[key] = e
	return acct
}

// snapshot returns the good readings for persistence.
func (c *summaryCache) snapshot() persistedCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := persistedCache{SavedAt: time.Now().UTC().Format(time.RFC3339), Accounts: map[string]persistedAccount{}}
	for key, e := range c.entries {
		if e.good != nil {
			out.Accounts[key] = persistedAccount{Account: *e.good, At: e.goodAt}
		}
	}
	return out
}

// seed installs persisted good readings as stale candidates only; nothing is
// served from them until a live fetch fails.
func (c *summaryCache) seed(p persistedCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for key, pa := range p.Accounts {
		if key == "" || pa.At.IsZero() || pa.Account.Error != "" {
			continue
		}
		e := c.entries[key]
		if e.good == nil || pa.At.After(e.goodAt) {
			good := pa.Account
			good.Stale, good.StaleReason = false, ""
			e.good, e.goodAt = &good, pa.At
			c.entries[key] = e
			n++
		}
	}
	return n
}

func (m *Manager) loadPersistedCache(cfg Config) {
	if cfg.resolvedCacheFile == "" {
		return
	}
	raw, err := os.ReadFile(cfg.resolvedCacheFile)
	if err != nil {
		return
	}
	var p persistedCache
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if n := m.cache.seed(p); n > 0 {
		m.log("info", "neuralwatt summary cache restored", map[string]any{"accounts": n})
	}
}

func (m *Manager) savePersistedCache(cfg Config) {
	if cfg.resolvedCacheFile == "" {
		return
	}
	snap := m.cache.snapshot()
	if len(snap.Accounts) == 0 {
		return
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	if string(raw) == m.lastPersisted {
		return
	}
	tmp := cfg.resolvedCacheFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, cfg.resolvedCacheFile); err != nil {
		_ = os.Remove(tmp)
		return
	}
	m.lastPersisted = string(raw)
}

var providerOrder = map[string]int{"claude": 0, "codex": 1, ProviderID: 2}

const summaryConcurrency = 4

// buildSummary lists every credential the host knows, fetches usage for the
// configured providers (cached per credential) and returns them in a stable
// order. Per-account failures are reported inline, never as a whole failure.
func (m *Manager) buildSummary(ctx context.Context, force bool) Summary {
	now := time.Now()
	out := Summary{Plugin: PluginID, Version: Version, GeneratedAt: now.UTC().Format(time.RFC3339), Accounts: []Account{}}
	if m.host == nil {
		out.Error = "host callbacks unavailable"
		return out
	}
	cfg := m.config()
	entries, err := m.host.AuthList(ctx)
	if err != nil {
		out.Error = "listing credentials failed"
		return out
	}
	type slot struct {
		entry pluginapi.HostAuthFileEntry
		acct  Account
	}
	slots := make([]*slot, 0, len(entries))
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if !cfg.summarizes(provider) || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		entry.Provider = provider
		slots = append(slots, &slot{entry: entry})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, summaryConcurrency)
	for _, s := range slots {
		ttl := cfg.CacheTTL()
		if force {
			ttl = summaryMinRefresh
		}
		if acct, ok := m.cache.get(s.entry.AuthIndex, ttl, now); ok {
			s.acct = acct
			continue
		}
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.acct = m.cache.record(s.entry.AuthIndex, m.fetchAccount(ctx, cfg, s.entry), time.Now())
		}(s)
	}
	wg.Wait()
	for _, s := range slots {
		out.Accounts = append(out.Accounts, s.acct)
	}
	m.savePersistedCache(cfg)
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		a, b := out.Accounts[i], out.Accounts[j]
		oa, oka := providerOrder[a.Provider]
		ob, okb := providerOrder[b.Provider]
		if !oka {
			oa = 99
		}
		if !okb {
			ob = 99
		}
		if oa != ob {
			return oa < ob
		}
		return a.Label < b.Label
	})
	return out
}

func (m *Manager) fetchAccount(ctx context.Context, cfg Config, entry pluginapi.HostAuthFileEntry) Account {
	acct := Account{
		Provider:    entry.Provider,
		AuthIndex:   entry.AuthIndex,
		Label:       strings.TrimSpace(entry.Label),
		Email:       strings.TrimSpace(entry.Email),
		Disabled:    entry.Disabled,
		Unavailable: entry.Unavailable,
		Windows:     []Window{},
		Metrics:     []pluginapi.QuotaMetric{},
	}
	defer func() {
		if r := recover(); r != nil {
			acct.Error = "internal error"
		}
	}()
	storage, err := m.loadStorage(ctx, entry)
	if err != nil {
		acct.Error = err.Error()
		return acct
	}
	var fetchErr error
	switch entry.Provider {
	case "claude":
		fetchErr = m.fetchClaude(ctx, cfg, storage, &acct)
	case "codex":
		fetchErr = m.fetchCodex(ctx, cfg, storage, &acct)
	case ProviderID:
		fetchErr = m.fetchNeuralwattAccount(ctx, cfg, storage, &acct)
	default:
		fetchErr = fmt.Errorf("provider %s has no usage source", entry.Provider)
	}
	if fetchErr != nil {
		acct.Error = fetchErr.Error()
		acct.rateLimited = strings.Contains(acct.Error, "429")
	}
	if acct.Label == "" {
		acct.Label = acct.Email
	}
	if acct.Label == "" {
		acct.Label = entry.Name
	}
	if acct.FetchedAt == "" {
		acct.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return acct
}

// loadStorage returns the credential's physical JSON via host.auth.get.
func (m *Manager) loadStorage(ctx context.Context, entry pluginapi.HostAuthFileEntry) (map[string]any, error) {
	resp, err := m.host.AuthGet(ctx, entry.AuthIndex)
	if err != nil {
		return nil, fmt.Errorf("reading credential failed")
	}
	var storage map[string]any
	if len(resp.JSON) == 0 || json.Unmarshal(resp.JSON, &storage) != nil {
		return nil, fmt.Errorf("credential storage is not valid JSON")
	}
	return storage, nil
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func numberField(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
