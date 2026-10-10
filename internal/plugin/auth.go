package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// authProvider owns the "neuralwatt" credential records. Records are created
// from plugin config at register time (see materializeCredential); there is no
// interactive login.
type authProvider struct{}

var _ pluginapi.AuthProvider = authProvider{}

// credentialRecord is the on-disk auth file shape saved through host.auth.save.
type credentialRecord struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Email  string `json:"email,omitempty"`
	APIKey string `json:"api_key"`
}

func (authProvider) Identifier() string { return ProviderID }

func (authProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var raw struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Label    string `json:"label"`
		Email    string `json:"email"`
		APIKey   string `json:"api_key"`
	}
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		if req.Provider == ProviderID {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("neuralwatt auth record has invalid JSON")
		}
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if (req.Provider != "" && req.Provider != ProviderID) || (raw.Type != ProviderID && raw.Provider != ProviderID) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	key := strings.TrimSpace(raw.APIKey)
	if key == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("neuralwatt auth record has no api key")
	}
	if raw.ID == "" {
		raw.ID = req.FileName
	}
	label := strings.TrimSpace(raw.Label)
	if label == "" {
		label = defaultLabel
	}
	attrs := map[string]string{"api_key": key}
	if email := strings.TrimSpace(raw.Email); email != "" {
		attrs["email"] = email
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          raw.ID,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: req.RawJSON,
		Attributes:  attrs,
	}}, nil
}

func (authProvider) StartLogin(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("neuralwatt login is unsupported; set api-key in plugins.configs.neuralwatt")
}

func (authProvider) PollLogin(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("neuralwatt login is unsupported; set api-key in plugins.configs.neuralwatt")
}

// RefreshAuth has nothing to refresh for a static API key; it echoes the record.
func (authProvider) RefreshAuth(_ context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	if req.AuthProvider != "" && req.AuthProvider != ProviderID {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("unsupported auth provider")
	}
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
		Provider:    ProviderID,
		ID:          req.AuthID,
		StorageJSON: req.StorageJSON,
		Metadata:    req.Metadata,
		Attributes:  req.Attributes,
	}}, nil
}
