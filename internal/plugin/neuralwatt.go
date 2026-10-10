package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// nwQuota mirrors GET /v1/quota on api.neuralwatt.com (fields observed 2026-10).
type nwQuota struct {
	SnapshotAt string `json:"snapshot_at"`
	Balance    struct {
		CreditsRemainingUSD float64 `json:"credits_remaining_usd"`
		TotalCreditsUSD     float64 `json:"total_credits_usd"`
		CreditsUsedUSD      float64 `json:"credits_used_usd"`
	} `json:"balance"`
	Usage struct {
		Lifetime     nwUsageBucket `json:"lifetime"`
		CurrentMonth nwUsageBucket `json:"current_month"`
	} `json:"usage"`
	Limits struct {
		OverageLimitUSD *float64 `json:"overage_limit_usd"`
		RateLimitTier   string   `json:"rate_limit_tier"`
	} `json:"limits"`
	Subscription struct {
		Plan               string  `json:"plan"`
		Status             string  `json:"status"`
		BillingInterval    string  `json:"billing_interval"`
		CurrentPeriodStart string  `json:"current_period_start"`
		CurrentPeriodEnd   string  `json:"current_period_end"`
		AutoRenew          bool    `json:"auto_renew"`
		KWhIncluded        float64 `json:"kwh_included"`
		KWhUsed            float64 `json:"kwh_used"`
		KWhRemaining       float64 `json:"kwh_remaining"`
		InOverage          bool    `json:"in_overage"`
		KWhResetDate       string  `json:"kwh_reset_date"`
	} `json:"subscription"`
	Key struct {
		Name      string   `json:"name"`
		Allowance *float64 `json:"allowance"`
	} `json:"key"`
}

type nwUsageBucket struct {
	CostUSD   float64 `json:"cost_usd"`
	Requests  float64 `json:"requests"`
	Tokens    float64 `json:"tokens"`
	EnergyKWh float64 `json:"energy_kwh"`
}

// errRateLimited marks an upstream 429; callers fall back to a cached reading.
var errRateLimited = fmt.Errorf("neuralwatt rate limited the quota request (http 429)")

// nwCache remembers the last good reading per key so CLIProxyBar-style
// polling (every 15 s) and the summary endpoint share one upstream call and
// a 429 never blanks the display.
type nwCache struct {
	mu      sync.Mutex
	entries map[string]nwCached
}

type nwCached struct {
	quota nwQuota
	at    time.Time
}

const (
	nwMinRefresh = 15 * time.Second // even a forced refresh waits this long
	nwStaleMax   = 15 * time.Minute // serve a stale reading this long on errors
)

func newNWCache() *nwCache { return &nwCache{entries: map[string]nwCached{}} }

func (c *nwCache) get(key string) (nwCached, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok
}

func (c *nwCache) put(key string, q nwQuota, at time.Time) {
	c.mu.Lock()
	c.entries[key] = nwCached{quota: q, at: at}
	c.mu.Unlock()
}

func (c *nwCache) reset() {
	c.mu.Lock()
	c.entries = map[string]nwCached{}
	c.mu.Unlock()
}

// neuralwattQuota returns a reading for key, from cache when fresh enough,
// otherwise from upstream; on upstream failure it serves a stale reading up
// to nwStaleMax old. The returned time is when the reading was taken.
func (m *Manager) neuralwattQuota(ctx context.Context, cfg Config, client httpDoer, key string, force bool) (nwQuota, time.Time, error) {
	digest := credentialID(key)
	now := time.Now()
	cached, ok := m.nwCache.get(digest)
	if ok {
		age := now.Sub(cached.at)
		if age < nwMinRefresh || (!force && age < cfg.CacheTTL()) {
			return cached.quota, cached.at, nil
		}
	}
	q, err := fetchNeuralwattQuota(ctx, client, cfg.BaseURL, key, cfg.Timeout())
	if err != nil {
		if ok && now.Sub(cached.at) < nwStaleMax {
			return cached.quota, cached.at, nil
		}
		return nwQuota{}, time.Time{}, err
	}
	m.nwCache.put(digest, q, now)
	return q, now, nil
}

type httpDoer interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// fetchNeuralwattQuota calls /v1/quota through the host HTTP client. Errors
// never include the key or the response body.
func fetchNeuralwattQuota(ctx context.Context, client httpDoer, baseURL, apiKey string, timeout time.Duration) (nwQuota, error) {
	var out nwQuota
	if client == nil {
		return out, fmt.Errorf("host http client is unavailable")
	}
	if strings.TrimSpace(apiKey) == "" {
		return out, fmt.Errorf("neuralwatt credential has no api key")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    strings.TrimRight(baseURL, "/") + "/v1/quota",
		Headers: http.Header{
			"Authorization": {"Bearer " + apiKey},
			"Accept":        {"application/json"},
			"User-Agent":    {"cliproxy-neuralwatt/" + Version},
		},
	})
	if err != nil {
		return out, fmt.Errorf("neuralwatt quota request failed")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return out, fmt.Errorf("neuralwatt rejected the api key (http %d)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return out, errRateLimited
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, fmt.Errorf("neuralwatt quota returned http %d", resp.StatusCode)
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return out, fmt.Errorf("neuralwatt quota response is not valid JSON")
	}
	if out.Subscription.KWhIncluded < 0 || math.IsNaN(out.Subscription.KWhIncluded) {
		return out, fmt.Errorf("neuralwatt quota response has invalid energy allowance")
	}
	return out, nil
}

// energyWindow returns the inferred length of the kWh period (the API only
// exposes the next reset; billing periods are monthly for every plan seen).
func energyWindow(q nwQuota, now time.Time) (resetAt time.Time, seconds int64, ok bool) {
	reset, err := time.Parse(time.RFC3339, q.Subscription.KWhResetDate)
	if err != nil {
		return time.Time{}, 0, false
	}
	start := reset.AddDate(0, -1, 0)
	if !start.Before(reset) {
		return time.Time{}, 0, false
	}
	return reset, int64(reset.Sub(start).Seconds()), true
}

// energyUsedPercent is the share of the included energy already charged.
func energyUsedPercent(q nwQuota) (float64, bool) {
	included := q.Subscription.KWhIncluded
	if included <= 0 {
		return 0, false
	}
	used := q.Subscription.KWhUsed
	if used < 0 {
		used = 0
	}
	return clampPercent(100 * used / included), true
}

func clampPercent(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// neuralwattQuotaResponse maps the API payload onto the host's generic quota
// contract: one energy bucket plus credit and spend summary metrics.
func neuralwattQuotaResponse(q nwQuota, now time.Time) pluginapi.QuotaFetchResponse {
	resp := pluginapi.QuotaFetchResponse{}
	if plan := strings.TrimSpace(q.Subscription.Plan); plan != "" {
		resp.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
	}
	if usedPct, ok := energyUsedPercent(q); ok {
		bucket := pluginapi.QuotaBucket{
			Window:            "Billing period",
			RemainingFraction: (100 - usedPct) / 100,
			Description:       fmt.Sprintf("%s of %s kWh", trimFloat(q.Subscription.KWhUsed), trimFloat(q.Subscription.KWhIncluded)),
		}
		if reset, _, ok := energyWindow(q, now); ok {
			bucket.ResetTime = reset.UTC().Format(time.RFC3339)
		}
		resp.Groups = []pluginapi.QuotaGroup{{DisplayName: "Energy", Buckets: []pluginapi.QuotaBucket{bucket}}}
	}
	resp.Summary = []pluginapi.QuotaMetric{
		{Key: "credits_remaining", Label: "Credits", Value: round2(q.Balance.CreditsRemainingUSD), Format: "currency", Currency: "USD"},
		{Key: "month_cost", Label: "This month", Value: round2(q.Usage.CurrentMonth.CostUSD), Format: "currency", Currency: "USD"},
		{Key: "month_requests", Label: "Requests this month", Value: q.Usage.CurrentMonth.Requests, Unit: "requests"},
		{Key: "kwh_remaining", Label: "Energy left", Value: round3(q.Subscription.KWhRemaining), Unit: "kWh"},
	}
	return resp
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
