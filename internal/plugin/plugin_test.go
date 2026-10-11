package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const neuralwattSample = `{"snapshot_at":"2026-10-10T13:04:37Z","balance":{"credits_remaining_usd":60.7167,"total_credits_usd":60.7167,"credits_used_usd":0.0},"usage":{"lifetime":{"cost_usd":210.8215,"requests":49917,"tokens":7169854132,"energy_kwh":43.8986},"current_month":{"cost_usd":8.4705,"requests":2994,"tokens":331007723,"energy_kwh":2.3703}},"limits":{"overage_limit_usd":null,"rate_limit_tier":"standard"},"subscription":{"plan":"standard","status":"active","billing_interval":"year","current_period_start":"2026-07-02T03:15:42Z","current_period_end":"2027-07-02T03:15:42Z","auto_renew":true,"kwh_included":16.0,"kwh_used":2.205,"kwh_remaining":13.795,"in_overage":false,"kwh_reset_date":"2026-10-30T03:41:02Z"},"key":{"name":"quotio-macpro","allowance":null}}`

const claudeSample = `{"five_hour":{"utilization":25.0,"resets_at":"2026-10-10T17:00:00.406136+00:00"},"seven_day":null,"limits":[{"kind":"session","group":"session","percent":25,"severity":"normal","resets_at":"2026-10-10T17:00:00.406136+00:00","scope":null,"is_active":true},{"kind":"weekly_scoped","group":"weekly","percent":3,"severity":"normal","resets_at":"2026-10-17T15:00:00.406358+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}]}`

const codexSample = `{"plan_type":"free","rate_limit":{"primary_window":{"used_percent":12,"reset_at":1760200000,"limit_window_seconds":18000},"secondary_window":{"used_percent":55,"reset_at":1760700000,"limit_window_seconds":604800}},"rate_limit_reset_credits":{"available_count":1}}`

// fakeHost is an in-memory HostClient.
type fakeHost struct {
	files     []pluginapi.HostAuthFileEntry
	storage   map[string]string
	saved     []pluginapi.HostAuthSaveRequest
	responses map[string]pluginapi.HTTPResponse
	requests  []pluginapi.HTTPRequest
}

func (f *fakeHost) AuthList(context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	return f.files, nil
}
func (f *fakeHost) AuthGet(_ context.Context, idx string) (pluginapi.HostAuthGetResponse, error) {
	return pluginapi.HostAuthGetResponse{AuthIndex: idx, JSON: json.RawMessage(f.storage[idx])}, nil
}
func (f *fakeHost) AuthSave(_ context.Context, req pluginapi.HostAuthSaveRequest) error {
	f.saved = append(f.saved, req)
	return nil
}
func (f *fakeHost) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.requests = append(f.requests, req)
	if r, ok := f.responses[req.URL]; ok {
		return r, nil
	}
	return pluginapi.HTTPResponse{StatusCode: 404}, nil
}
func (f *fakeHost) Log(string, string, map[string]any) {}

func TestNeuralwattMapping(t *testing.T) {
	var q nwQuota
	if err := json.Unmarshal([]byte(neuralwattSample), &q); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	resp := neuralwattQuotaResponse(q, now)
	if resp.Subscription == nil || resp.Subscription.Plan != "standard" {
		t.Fatalf("plan = %+v", resp.Subscription)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", resp.Groups)
	}
	b := resp.Groups[0].Buckets[0]
	if b.RemainingFraction < 0.861 || b.RemainingFraction > 0.863 {
		t.Fatalf("remainingFraction = %v", b.RemainingFraction)
	}
	if b.ResetTime != "2026-10-30T03:41:02Z" {
		t.Fatalf("resetTime = %q", b.ResetTime)
	}
	if b.Description != "2.21 of 16 kWh" && b.Description != "2.2 of 16 kWh" {
		t.Fatalf("description = %q", b.Description)
	}
	var credits, month float64
	for _, m := range resp.Summary {
		switch m.Key {
		case "credits_remaining":
			credits = m.Value
			if m.Format != "currency" || m.Currency != "USD" {
				t.Fatalf("credits metric = %+v", m)
			}
		case "month_cost":
			month = m.Value
		}
	}
	if credits != 60.72 || month != 8.47 {
		t.Fatalf("credits=%v month=%v", credits, month)
	}
	_, seconds, ok := energyWindow(q, now)
	if !ok || seconds != int64((30*24*time.Hour).Seconds()) {
		t.Fatalf("energy window seconds = %d ok=%v", seconds, ok)
	}
}

func TestClaudeWindowsUseLimitsArray(t *testing.T) {
	var u claudeUsage
	if err := json.Unmarshal([]byte(claudeSample), &u); err != nil {
		t.Fatal(err)
	}
	w := claudeWindows(u)
	if len(w) != 2 {
		t.Fatalf("windows = %+v", w)
	}
	if w[0].Key != "session" || w[0].UsedPercent != 25 || w[0].WindowSeconds != sessionSeconds || w[0].Severity != "normal" {
		t.Fatalf("session = %+v", w[0])
	}
	if w[1].Key != "weekly_fable" || w[1].Label != "Weekly · Fable" || w[1].UsedPercent != 3 || w[1].WindowSeconds != weekSeconds {
		t.Fatalf("weekly = %+v", w[1])
	}
	if organizationDisplay("seat-two@example.com's Organization") != "Personal" || organizationDisplay("Inreach") != "Inreach" {
		t.Fatal("organization display")
	}
}

func TestNeuralwattCacheServesStaleOn429(t *testing.T) {
	host := &fakeHost{responses: map[string]pluginapi.HTTPResponse{
		defaultBaseURL + "/v1/quota": {StatusCode: 200, Body: []byte(neuralwattSample)},
	}}
	m := NewManager(host)
	cfg := m.config()
	q, _, err := m.neuralwattQuota(context.Background(), cfg, host, "sk-x", false)
	if err != nil || q.Subscription.Plan != "standard" {
		t.Fatalf("first fetch: %v %+v", err, q.Subscription)
	}
	host.responses[defaultBaseURL+"/v1/quota"] = pluginapi.HTTPResponse{StatusCode: 429}
	// Within nwMinRefresh even a forced refresh is served from cache.
	if _, _, err := m.neuralwattQuota(context.Background(), cfg, host, "sk-x", true); err != nil || len(host.requests) != 1 {
		t.Fatalf("forced refresh hit upstream: err=%v requests=%d", err, len(host.requests))
	}
	// Age the entry past the TTL: upstream 429 must fall back to the stale reading.
	m.nwCache.put(credentialID("sk-x"), q, time.Now().Add(-2*cfg.CacheTTL()))
	q2, at, err := m.neuralwattQuota(context.Background(), cfg, host, "sk-x", false)
	if err != nil || q2.Subscription.Plan != "standard" || len(host.requests) != 2 || time.Since(at) < cfg.CacheTTL() {
		t.Fatalf("stale fallback: err=%v requests=%d at=%v", err, len(host.requests), at)
	}
	// Beyond nwStaleMax the error surfaces.
	m.nwCache.put(credentialID("sk-x"), q, time.Now().Add(-2*nwStaleMax))
	if _, _, err := m.neuralwattQuota(context.Background(), cfg, host, "sk-x", false); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got %v", err)
	}
}

func TestCodexMonthlyWindow(t *testing.T) {
	var u codexUsage
	_ = json.Unmarshal([]byte(`{"plan_type":"free","rate_limit":{"secondary_window":{"used_percent":68,"reset_at":1762420392,"limit_window_seconds":2592000}}}`), &u)
	w := codexWindows(u)
	if len(w) != 1 || w[0].Key != "monthly" || w[0].Label != "Monthly" {
		t.Fatalf("windows = %+v", w)
	}
}

func TestCodexWindows(t *testing.T) {
	var u codexUsage
	if err := json.Unmarshal([]byte(codexSample), &u); err != nil {
		t.Fatal(err)
	}
	w := codexWindows(u)
	if len(w) != 2 || w[0].Key != "session" || w[1].Key != "weekly" || w[1].UsedPercent != 55 {
		t.Fatalf("windows = %+v", w)
	}
	if w[0].ResetsAt != time.Unix(1760200000, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("resets_at = %q", w[0].ResetsAt)
	}
}

func TestRegisterMaterializesCredentialOnce(t *testing.T) {
	host := &fakeHost{}
	m := NewManager(host)
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\npriority: 0\napi-key: \"sk-test-123\"\nlabel: \"NW\"\n")})
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, req)
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("register envelope = %s", raw)
	}
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Capabilities  map[string]any
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil || reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("registration = %s", env.Result)
	}
	if reg.Capabilities["quota_provider"] != true || reg.Capabilities["auth_provider"] != true || reg.Capabilities["executor"] != false {
		t.Fatalf("capabilities = %v", reg.Capabilities)
	}
	if len(host.saved) != 1 || !strings.HasPrefix(host.saved[0].Name, "neuralwatt-") || !strings.HasSuffix(host.saved[0].Name, ".json") {
		t.Fatalf("saved = %+v", host.saved)
	}
	var rec credentialRecord
	if err := json.Unmarshal(host.saved[0].JSON, &rec); err != nil || rec.APIKey != "sk-test-123" || rec.Type != ProviderID || rec.Label != "NW" {
		t.Fatalf("record = %+v", rec)
	}
	// Second register with the credential now listed must not save again.
	host.files = []pluginapi.HostAuthFileEntry{{ID: rec.ID, Name: host.saved[0].Name, Provider: ProviderID}}
	if _, err := m.HandleCall(pluginabi.MethodPluginReconfigure, req); err != nil {
		t.Fatal(err)
	}
	if len(host.saved) != 1 {
		t.Fatalf("credential saved twice: %+v", host.saved)
	}
}

func TestAuthParseAndQuotaFetch(t *testing.T) {
	host := &fakeHost{responses: map[string]pluginapi.HTTPResponse{
		defaultBaseURL + "/v1/quota": {StatusCode: 200, Body: []byte(neuralwattSample)},
	}}
	m := NewManager(host)
	parseReq, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "", FileName: "neuralwatt-abc.json", RawJSON: []byte(`{"type":"neuralwatt","id":"neuralwatt-abc","label":"NW","api_key":"sk-x"}`)})
	raw, _ := m.HandleCall(pluginabi.MethodAuthParse, parseReq)
	var env pluginabi.Envelope
	_ = json.Unmarshal(raw, &env)
	var parsed pluginapi.AuthParseResponse
	if !env.OK || json.Unmarshal(env.Result, &parsed) != nil || !parsed.Handled || parsed.Auth.Attributes["api_key"] != "sk-x" || parsed.Auth.Provider != ProviderID {
		t.Fatalf("parse = %s", raw)
	}
	// Foreign records are not handled.
	foreign, _ := json.Marshal(pluginapi.AuthParseRequest{FileName: "claude-x.json", RawJSON: []byte(`{"type":"claude","access_token":"t"}`)})
	raw, _ = m.HandleCall(pluginabi.MethodAuthParse, foreign)
	_ = json.Unmarshal(raw, &env)
	_ = json.Unmarshal(env.Result, &parsed)
	if !env.OK || parsed.Handled {
		t.Fatalf("foreign parse = %s", raw)
	}
	fetchReq, _ := json.Marshal(pluginapi.QuotaFetchRequest{AuthIndex: "idx", Provider: ProviderID, Attributes: map[string]string{"api_key": "sk-x"}})
	raw, _ = m.HandleCall(pluginabi.MethodQuotaFetch, fetchReq)
	_ = json.Unmarshal(raw, &env)
	var q pluginapi.QuotaFetchResponse
	if !env.OK || json.Unmarshal(env.Result, &q) != nil || len(q.Groups) != 1 {
		t.Fatalf("fetch = %s", raw)
	}
	if got := host.requests[0].Headers.Get("Authorization"); got != "Bearer sk-x" {
		t.Fatalf("auth header = %q", got)
	}
}

func TestSummaryEndpoint(t *testing.T) {
	host := &fakeHost{
		files: []pluginapi.HostAuthFileEntry{
			{AuthIndex: "c1", Provider: "claude", Label: "seat-one@example.com", Email: "seat-one@example.com"},
			{AuthIndex: "x1", Provider: "codex", Label: "seat-two@example.com", Email: "seat-two@example.com"},
			{AuthIndex: "n1", Provider: "neuralwatt", Label: "Neuralwatt"},
			{AuthIndex: "g1", Provider: "gemini", Label: "ignored"},
		},
		storage: map[string]string{
			"c1": `{"type":"claude","access_token":"tok","email":"seat-one@example.com","organization_name":"Inreach"}`,
			"x1": `{"type":"codex","access_token":"tok2","account_id":"acc","email":"seat-two@example.com"}`,
			"n1": `{"type":"neuralwatt","api_key":"sk-x"}`,
		},
		responses: map[string]pluginapi.HTTPResponse{
			anthropicUsageURL + anthropicResetQuery: {StatusCode: 200, Body: []byte(claudeSample)},
			codexUsageURL:                           {StatusCode: 200, Body: []byte(codexSample)},
			defaultBaseURL + "/v1/quota":            {StatusCode: 200, Body: []byte(neuralwattSample)},
		},
	}
	m := NewManager(host)
	req := pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBase + "/summary"}
	resp, err := m.HandleManagement(context.Background(), req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp = %+v err=%v", resp, err)
	}
	var s Summary
	if err := json.Unmarshal(resp.Body, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts) != 3 {
		t.Fatalf("accounts = %+v", s.Accounts)
	}
	if s.Accounts[0].Provider != "claude" || s.Accounts[0].Label != "Inreach · seat-one@example.com" || len(s.Accounts[0].Windows) != 2 {
		t.Fatalf("claude = %+v", s.Accounts[0])
	}
	if s.Accounts[1].Provider != "codex" || s.Accounts[1].Plan != "free" || len(s.Accounts[1].Windows) != 2 {
		t.Fatalf("codex = %+v", s.Accounts[1])
	}
	if s.Accounts[2].Provider != "neuralwatt" || len(s.Accounts[2].Windows) != 1 || s.Accounts[2].Windows[0].Key != "energy" || len(s.Accounts[2].Metrics) != 4 {
		t.Fatalf("neuralwatt = %+v", s.Accounts[2])
	}
	for _, r := range host.requests {
		if r.URL == codexUsageURL && r.Headers.Get("ChatGPT-Account-Id") != "acc" {
			t.Fatalf("codex headers = %v", r.Headers)
		}
	}
	// Cached: a second call makes no new upstream requests.
	n := len(host.requests)
	if _, err := m.HandleManagement(context.Background(), req); err != nil || len(host.requests) != n {
		t.Fatalf("cache miss: %d -> %d", n, len(host.requests))
	}
	// Upstream 429 after the cache expires: the last good reading is served, flagged stale.
	host.responses[anthropicUsageURL+anthropicResetQuery] = pluginapi.HTTPResponse{StatusCode: 429}
	m.cache.mu.Lock()
	for k, e := range m.cache.entries {
		e.lastAt = e.lastAt.Add(-10 * time.Minute)
		e.goodAt = e.goodAt.Add(-10 * time.Minute)
		m.cache.entries[k] = e
	}
	m.cache.mu.Unlock()
	resp, _ = m.HandleManagement(context.Background(), req)
	_ = json.Unmarshal(resp.Body, &s)
	if !s.Accounts[0].Stale || s.Accounts[0].Error != "" || len(s.Accounts[0].Windows) != 2 || !strings.Contains(s.Accounts[0].StaleReason, "429") {
		t.Fatalf("stale fallback = %+v", s.Accounts[0])
	}
	// While backing off after the 429, a forced refresh makes no upstream call for that account.
	n = len(host.requests)
	resp, _ = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBase + "/summary", Query: map[string][]string{"refresh": {"1"}}})
	var again Summary
	_ = json.Unmarshal(resp.Body, &again)
	if len(host.requests) != n || !again.Accounts[0].Stale {
		t.Fatalf("backoff not honoured: requests %d -> %d, stale=%v", n, len(host.requests), again.Accounts[0].Stale)
	}
	page, _ := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: resourceBase + "/status"})
	if page.StatusCode != 200 || !strings.Contains(string(page.Body), "neuralwatt/summary") {
		t.Fatalf("status page = %d", page.StatusCode)
	}
}

func TestPersistedCacheSeedsStaleFallback(t *testing.T) {
	dir := t.TempDir()
	cacheFile := dir + "/cache.json"
	host := &fakeHost{
		files:     []pluginapi.HostAuthFileEntry{{AuthIndex: "c1", Provider: "claude", Label: "x", Email: "seat-one@example.com"}},
		storage:   map[string]string{"c1": `{"type":"claude","access_token":"tok","email":"seat-one@example.com","organization_name":"Inreach"}`},
		responses: map[string]pluginapi.HTTPResponse{anthropicUsageURL + anthropicResetQuery: {StatusCode: 200, Body: []byte(claudeSample)}},
	}
	m := NewManager(host)
	cfgYAML, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\ncache-file: \"" + cacheFile + "\"\n")})
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, cfgYAML); err != nil {
		t.Fatal(err)
	}
	req := pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementBase + "/summary"}
	if _, err := m.HandleManagement(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	// A fresh manager (restart) with upstream now rate-limited serves the persisted reading as stale.
	host.responses[anthropicUsageURL+anthropicResetQuery] = pluginapi.HTTPResponse{StatusCode: 429}
	m2 := NewManager(host)
	if _, err := m2.HandleCall(pluginabi.MethodPluginRegister, cfgYAML); err != nil {
		t.Fatal(err)
	}
	resp, _ := m2.HandleManagement(context.Background(), req)
	var s Summary
	_ = json.Unmarshal(resp.Body, &s)
	if len(s.Accounts) != 1 || !s.Accounts[0].Stale || len(s.Accounts[0].Windows) != 2 || s.Accounts[0].Error != "" {
		t.Fatalf("restart fallback = %+v", s.Accounts)
	}
}

const claudeResetsSample = `{"five_hour":{"utilization":25.0,"resets_at":"2026-10-11T15:10:00Z"},
"limits":[{"kind":"session","group":"session","percent":25,"severity":"normal","resets_at":"2026-10-11T15:10:00Z","is_active":true}],
"cedar_ember":{"eligible":true,"ineligible_reason":"","at_limit":false,"grants":[
 {"id":"g1","label":"Launch","resets_total":2,"resets_left":1,"starts_at":"2026-10-01T00:00:00Z","ends_at":"2026-10-20T00:00:00Z"},
 {"id":"g2","label":"Promo","resets_total":1,"resets_left":0,"starts_at":"2026-10-01T00:00:00Z","ends_at":"2026-10-15T00:00:00Z"},
 {"id":"g3","label":"Later","resets_total":3,"resets_left":2,"starts_at":"2026-10-01T00:00:00Z","ends_at":"2026-11-30T00:00:00Z"}]}}`

func TestClaudeLimitResets(t *testing.T) {
	var u claudeUsage
	if err := json.Unmarshal([]byte(claudeResetsSample), &u); err != nil {
		t.Fatal(err)
	}
	lr := limitResets(u.CedarEmber)
	if lr == nil || !lr.Eligible || lr.Available != 3 || lr.ExpiresAt != "2026-10-20T00:00:00Z" {
		t.Fatalf("limit resets = %+v", lr)
	}
	var ineligible claudeUsage
	if err := json.Unmarshal([]byte(`{"cedar_ember":{"eligible":false,"ineligible_reason":"tenure","grants":[]}}`), &ineligible); err != nil {
		t.Fatal(err)
	}
	lr = limitResets(ineligible.CedarEmber)
	if lr == nil || lr.Eligible || lr.Available != 0 || lr.Reason != "tenure" || lr.ExpiresAt != "" {
		t.Fatalf("ineligible = %+v", lr)
	}
	if limitResets(nil) != nil {
		t.Fatal("absent block must stay nil")
	}
}
