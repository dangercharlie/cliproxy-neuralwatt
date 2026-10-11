package plugin

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	managementBase = "/v0/management/plugins/" + PluginID
	resourceBase   = "/v0/resource/plugins/" + PluginID
	summaryTimeout = 25 * time.Second
)

//go:embed resources/status.html
var statusPage string

//go:embed resources/logo.svg
var logoSVG string

type routeSpec struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type resourceSpec struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

// registerManagement declares the management routes (key-protected JSON) and
// browser resources (served without the management middleware; the page asks
// for the key itself and stores it in localStorage).
func (m *Manager) registerManagement(_ []byte) ([]byte, error) {
	return okEnvelope(struct {
		Routes    []routeSpec    `json:"routes"`
		Resources []resourceSpec `json:"resources"`
	}{
		Routes: []routeSpec{
			{Method: http.MethodGet, Path: "/plugins/" + PluginID + "/summary"},
			{Method: http.MethodGet, Path: "/plugins/" + PluginID + "/upstream"},
		},
		Resources: []resourceSpec{
			{Path: "/status", Menu: "Quota status", Description: "Claude, Codex and Neuralwatt quota in one page (phone friendly)."},
			{Path: "/logo.svg"},
			{Path: "/quota.json", Description: "Read-only summary without the management key (summary.public)."},
		},
	}), nil
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), summaryTimeout)
	defer cancel()
	resp, err := m.HandleManagement(ctx, req)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

var _ pluginapi.ManagementHandler = (*Manager)(nil)

func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	path := strings.TrimRight(req.Path, "/")
	switch {
	case req.Method == http.MethodGet && path == resourceBase+"/status":
		return htmlResponse(statusPage), nil
	case req.Method == http.MethodGet && path == resourceBase+"/logo.svg":
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"image/svg+xml"}, "Cache-Control": {"public, max-age=86400"}}, Body: []byte(logoSVG)}, nil
	case req.Method == http.MethodGet && path == resourceBase+"/quota.json":
		// Key-free, so it never forces a refresh: callers get the cached reading.
		if !m.config().Summary.Public {
			return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}), nil
		}
		return jsonResponse(http.StatusOK, m.buildSummary(ctx, false)), nil
	case req.Method == http.MethodGet && path == managementBase+"/summary":
		force := req.Query != nil && req.Query.Get("refresh") != ""
		return jsonResponse(http.StatusOK, m.buildSummary(ctx, force)), nil
	case req.Method == http.MethodGet && path == managementBase+"/upstream":
		return m.rawNeuralwattQuota(ctx), nil
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"}), nil
}

// rawNeuralwattQuota returns the upstream payload for the configured key next
// to the host-contract mapping. Useful for debugging the mapping.
func (m *Manager) rawNeuralwattQuota(ctx context.Context) pluginapi.ManagementResponse {
	cfg := m.config()
	key := cfg.APIKey
	if key == "" {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "no api-key configured"})
	}
	q, at, err := m.neuralwattQuota(ctx, cfg, m.host, key, false)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"fetched_at": at.UTC().Format(time.RFC3339), "upstream": q, "quota": neuralwattQuotaResponse(q, time.Now())})
}

func jsonResponse(status int, body any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(body)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}},
		Body:       raw,
	}
}

// statusCSP lets the page run only its own inline script (by hash), talk only
// to this origin, and refuses framing. The page reads the management key, so a
// script that slipped in could not send it elsewhere.
var statusCSP = func() string {
	script := statusPage
	if i := strings.Index(script, "<script>"); i >= 0 {
		script = script[i+len("<script>"):]
	}
	if i := strings.Index(script, "</script>"); i >= 0 {
		script = script[:i]
	}
	sum := sha256.Sum256([]byte(script))
	return "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

func htmlResponse(page string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"Content-Security-Policy": {statusCSP},
			"X-Frame-Options":         {"DENY"},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
		},
		Body: []byte(page),
	}
}
