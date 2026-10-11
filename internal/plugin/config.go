package plugin

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultBaseURL        = "https://api.neuralwatt.com"
	defaultLabel          = "Neuralwatt"
	defaultCacheSeconds   = 90
	defaultTimeoutSeconds = 15
	defaultCacheFile      = "plugins/neuralwatt-cache.json"
)

var defaultSummaryProviders = []string{"claude", "codex", ProviderID}

// Config is the plugins.configs.neuralwatt block as the host hands it over
// (YAML, with enabled/priority added by the host).
type Config struct {
	// resolvedCacheFile is CacheFile after defaults ("" disables persistence).
	resolvedCacheFile string

	Enabled        bool    `yaml:"enabled"`
	Priority       int     `yaml:"priority"`
	APIKey         string  `yaml:"api-key"`
	APIKeyEnv      string  `yaml:"api-key-env"`
	APIKeyFile     string  `yaml:"api-key-file"`
	Label          string  `yaml:"label"`
	BaseURL        string  `yaml:"base-url"`
	CacheSeconds   int     `yaml:"cache-seconds"`
	TimeoutSeconds int     `yaml:"timeout-seconds"`
	CacheFile      *string `yaml:"cache-file"`
	Summary        struct {
		Providers []string `yaml:"providers"`
		// Public serves the summary at the key-free quota.json resource, for
		// home-screen widgets. Off by default: the summary names accounts.
		Public bool `yaml:"public"`
	} `yaml:"summary"`
}

func (c Config) Timeout() time.Duration  { return time.Duration(c.TimeoutSeconds) * time.Second }
func (c Config) CacheTTL() time.Duration { return time.Duration(c.CacheSeconds) * time.Second }

// LoadConfig parses the YAML block, resolves the API key (literal, env var,
// or file) and applies defaults. It never echoes the key in errors.
func LoadConfig(raw []byte) (Config, error) {
	var cfg Config
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("plugin config is not valid YAML")
		}
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.APIKey == "" && strings.TrimSpace(cfg.APIKeyEnv) != "" {
		cfg.APIKey = strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.APIKeyEnv)))
	}
	if cfg.APIKey == "" && strings.TrimSpace(cfg.APIKeyFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(cfg.APIKeyFile))
		if err != nil {
			return Config{}, fmt.Errorf("api-key-file could not be read")
		}
		cfg.APIKey = strings.TrimSpace(string(data))
	}
	if strings.ContainsAny(cfg.APIKey, "\r\n\x00") {
		return Config{}, fmt.Errorf("api-key contains invalid characters")
	}
	if strings.TrimSpace(cfg.Label) == "" {
		cfg.Label = defaultLabel
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, fmt.Errorf("base-url must be an https URL without credentials, query or fragment")
	}
	if cfg.CacheSeconds <= 0 {
		cfg.CacheSeconds = defaultCacheSeconds
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = defaultTimeoutSeconds
	}
	cfg.resolvedCacheFile = defaultCacheFile
	if cfg.CacheFile != nil {
		cfg.resolvedCacheFile = strings.TrimSpace(*cfg.CacheFile)
	}
	if len(cfg.Summary.Providers) == 0 {
		cfg.Summary.Providers = append([]string(nil), defaultSummaryProviders...)
	}
	for i, p := range cfg.Summary.Providers {
		cfg.Summary.Providers[i] = strings.ToLower(strings.TrimSpace(p))
	}
	return cfg, nil
}

func (c Config) summarizes(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, p := range c.Summary.Providers {
		if p == provider {
			return true
		}
	}
	return false
}
