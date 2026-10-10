package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var _ pluginapi.QuotaProvider = (*Manager)(nil)

func (m *Manager) Identifier() string { return ProviderID }

func (m *Manager) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{ProviderID},
		DisplayName:        m.config().Label,
		SupportsReset:      false,
	}, nil
}

// FetchQuota serves the host's generic quota contract for one neuralwatt
// credential. The key comes from the credential the host selected, never from
// plugin config, so several keys can coexist.
func (m *Manager) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.Provider != "" && strings.ToLower(req.Provider) != ProviderID {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("unsupported quota provider")
	}
	key := strings.TrimSpace(req.Attributes["api_key"])
	if key == "" && len(req.StorageJSON) > 0 {
		var rec credentialRecord
		if json.Unmarshal(req.StorageJSON, &rec) == nil {
			key = strings.TrimSpace(rec.APIKey)
		}
	}
	if key == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota credential has no api key")
	}
	cfg := m.config()
	var client httpDoer = m.host
	if req.HTTPClient != nil {
		client = req.HTTPClient
	}
	q, _, err := m.neuralwattQuota(ctx, cfg, client, key, false)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	return neuralwattQuotaResponse(q, time.Now()), nil
}

func (m *Manager) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "neuralwatt quota cannot be reset"}, fmt.Errorf("unsupported")
}
