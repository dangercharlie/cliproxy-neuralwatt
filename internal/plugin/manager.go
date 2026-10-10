// Package plugin implements the CLIProxyAPI v8 native plugin "neuralwatt":
// a quota-only provider (auth provider + quota provider, no executor) for
// Neuralwatt, plus a management endpoint that summarises quota across every
// credential the proxy holds and a phone-friendly status page.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// PluginID is the library file name (neuralwatt.so) and the config key
	// under plugins.configs.
	PluginID = "neuralwatt"
	// ProviderID is the auth/quota provider identifier.
	ProviderID = "neuralwatt"
	Version    = "0.1.0"
	repoURL    = "https://github.com/dangercharlie/cliproxy-neuralwatt"

	lifecycleTimeout = 10 * time.Second
	shutdownGrace    = 5 * time.Second
)

// Manager dispatches RPC methods from the host.
type Manager struct {
	host    HostClient
	mu      sync.RWMutex
	cfg     Config
	lifeMu  sync.Mutex
	cache   *summaryCache
	nwCache *nwCache
}

func NewManager(host HostClient) *Manager {
	cfg, _ := LoadConfig(nil)
	return &Manager{host: host, cfg: cfg, cache: newSummaryCache(), nwCache: newNWCache()}
}

func (m *Manager) config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{}), nil
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		out, errParse := (authProvider{}).ParseAuth(context.Background(), req)
		if errParse != nil {
			return ErrEnvelope("auth_failure", errParse.Error()), nil
		}
		return okEnvelope(out), nil
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return ErrEnvelope("unsupported", "neuralwatt login is unsupported; set api-key in plugins.configs.neuralwatt"), nil
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		out, errRefresh := (authProvider{}).RefreshAuth(context.Background(), req)
		if errRefresh != nil {
			return ErrEnvelope("auth_failure", errRefresh.Error()), nil
		}
		return okEnvelope(out), nil

	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodQuotaDescribe:
		var req pluginapi.QuotaDescribeRequest
		_ = json.Unmarshal(request, &req)
		out, _ := m.DescribeQuota(context.Background(), req)
		return okEnvelope(out), nil
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed quota fetch request body"), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), m.config().Timeout()+2*time.Second)
		defer cancel()
		out, errFetch := m.FetchQuota(ctx, req)
		if errFetch != nil {
			return ErrEnvelope("quota_failure", errFetch.Error()), nil
		}
		return okEnvelope(out), nil
	case pluginabi.MethodQuotaReset:
		return ErrEnvelope("unsupported", "neuralwatt quota cannot be reset"), nil

	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	AuthProvider  bool `json:"auth_provider"`
	Executor      bool `json:"executor"`
	QuotaProvider bool `json:"quota_provider"`
	ManagementAPI bool `json:"management_api"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "api-key", Type: pluginapi.ConfigFieldTypeString, Description: "Neuralwatt API key (sk-…). Alternatively api-key-env or api-key-file."},
		{Name: "api-key-env", Type: pluginapi.ConfigFieldTypeString, Description: "Environment variable holding the API key."},
		{Name: "api-key-file", Type: pluginapi.ConfigFieldTypeString, Description: "File holding the API key."},
		{Name: "label", Type: pluginapi.ConfigFieldTypeString, Description: "Display label for the credential (default Neuralwatt)."},
		{Name: "base-url", Type: pluginapi.ConfigFieldTypeString, Description: "Neuralwatt API base URL (default https://api.neuralwatt.com)."},
		{Name: "cache-seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "How long summary results are cached per credential (default 45)."},
		{Name: "timeout-seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Upstream request timeout (default 15)."},
	}
}

func registrationEnvelope() []byte {
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Neuralwatt",
			Version:          Version,
			Author:           "dangercharlie",
			GitHubRepository: repoURL,
			Logo:             resourceBase + "/logo.svg",
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{AuthProvider: true, Executor: false, QuotaProvider: true, ManagementAPI: true},
	})
}

func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(request) > 0 && json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := LoadConfig(req.ConfigYAML)
	if err != nil {
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
	m.cache.reset()
	m.nwCache.reset()
	if cfg.Enabled && cfg.APIKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()
		if errMat := m.materializeCredential(ctx, cfg); errMat != nil {
			return ErrEnvelope("auth_materialization_failed", errMat.Error()), nil
		}
	} else if cfg.Enabled {
		m.log("warn", "neuralwatt plugin enabled without an api-key; no credential was created", nil)
	}
	return registrationEnvelope(), nil
}

// materializeCredential makes the configured key visible to CPA as an auth
// file, idempotently. Existing records are discovered through the host so
// their metadata is never overwritten. The ID is derived from the key digest,
// which is non-secret and stable across restarts.
func (m *Manager) materializeCredential(ctx context.Context, cfg Config) error {
	if m.host == nil {
		return fmt.Errorf("host callbacks unavailable")
	}
	entries, err := m.host.AuthList(ctx)
	if err != nil {
		return fmt.Errorf("list existing auth records failed")
	}
	id := credentialID(cfg.APIKey)
	name := id + ".json"
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == id || strings.TrimSpace(entry.Name) == name {
			return nil
		}
	}
	record, err := json.Marshal(credentialRecord{Type: ProviderID, ID: id, Label: cfg.Label, APIKey: cfg.APIKey})
	if err != nil {
		return fmt.Errorf("build auth record failed")
	}
	if err := m.host.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: name, JSON: record}); err != nil {
		return err
	}
	m.log("info", "neuralwatt credential created", map[string]any{"id": id})
	return nil
}

func credentialID(apiKey string) string {
	digest := sha256.Sum256([]byte(apiKey))
	return "neuralwatt-" + hex.EncodeToString(digest[:])[:12]
}

func (m *Manager) handleShutdown() ([]byte, error) {
	if bridge, ok := m.host.(*HostBridge); ok && bridge != nil {
		bridge.Wait(shutdownGrace)
	}
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) log(level, message string, fields map[string]any) {
	if m.host == nil {
		return
	}
	m.host.Log(level, message, fields)
}
