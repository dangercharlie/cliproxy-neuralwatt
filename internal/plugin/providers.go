package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	anthropicUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL     = "https://chatgpt.com/backend-api/wham/usage"
	sessionSeconds    = 5 * 60 * 60
	weekSeconds       = 7 * 24 * 60 * 60
)

func (m *Manager) getJSON(ctx context.Context, cfg Config, url string, headers http.Header, what string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout())
	defer cancel()
	if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", "cliproxy-neuralwatt/"+Version)
	}
	resp, err := m.host.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: url, Headers: headers})
	if err != nil {
		return nil, fmt.Errorf("%s request failed", what)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%s rejected the credential (http %d)", what, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s returned http %d", what, resp.StatusCode)
	}
	return resp.Body, nil
}

// --- Claude (Anthropic OAuth usage) ---------------------------------------

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeLimit struct {
	Kind     string   `json:"kind"`
	Group    string   `json:"group"`
	Percent  *float64 `json:"percent"`
	Severity string   `json:"severity"`
	ResetsAt string   `json:"resets_at"`
	IsActive bool     `json:"is_active"`
	Scope    *struct {
		Model *struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"model"`
		Surface string `json:"surface"`
	} `json:"scope"`
}

type claudeUsage struct {
	FiveHour       *claudeWindow `json:"five_hour"`
	SevenDay       *claudeWindow `json:"seven_day"`
	SevenDayOpus   *claudeWindow `json:"seven_day_opus"`
	SevenDaySonnet *claudeWindow `json:"seven_day_sonnet"`
	Limits         []claudeLimit `json:"limits"`
}

func (m *Manager) fetchClaude(ctx context.Context, cfg Config, storage map[string]any, acct *Account) error {
	token := stringField(storage, "access_token")
	if token == "" {
		return fmt.Errorf("credential has no access token")
	}
	if email := stringField(storage, "email"); email != "" {
		acct.Email = email
	}
	acct.Organization = organizationDisplay(stringField(storage, "organization_name"))
	acct.Label = claudeLabel(acct.Organization, acct.Email)
	body, err := m.getJSON(ctx, cfg, anthropicUsageURL, http.Header{
		"Authorization":  {"Bearer " + token},
		"anthropic-beta": {"oauth-2025-04-20"},
	}, "anthropic usage")
	if err != nil {
		return err
	}
	var usage claudeUsage
	if json.Unmarshal(body, &usage) != nil {
		return fmt.Errorf("anthropic usage response is not valid JSON")
	}
	acct.Windows = claudeWindows(usage)
	if len(acct.Windows) == 0 {
		return fmt.Errorf("anthropic usage response carried no limits")
	}
	return nil
}

func claudeWindows(usage claudeUsage) []Window {
	out := make([]Window, 0, 4)
	if usage.FiveHour != nil && usage.FiveHour.Utilization != nil {
		out = append(out, Window{Key: "session", Label: "5 hours", UsedPercent: clampPercent(*usage.FiveHour.Utilization), ResetsAt: usage.FiveHour.ResetsAt, WindowSeconds: sessionSeconds})
	}
	if len(usage.Limits) > 0 {
		for _, limit := range usage.Limits {
			if limit.Percent == nil {
				continue
			}
			switch limit.Kind {
			case "session":
				if len(out) > 0 && out[0].Key == "session" {
					out[0].Severity = limit.Severity
					continue
				}
				out = append(out, Window{Key: "session", Label: "5 hours", UsedPercent: clampPercent(*limit.Percent), ResetsAt: limit.ResetsAt, WindowSeconds: sessionSeconds, Severity: limit.Severity})
			case "weekly_all":
				out = append(out, Window{Key: "weekly", Label: "Weekly", UsedPercent: clampPercent(*limit.Percent), ResetsAt: limit.ResetsAt, WindowSeconds: weekSeconds, Severity: limit.Severity})
			case "weekly_scoped":
				scope := ""
				if limit.Scope != nil && limit.Scope.Model != nil {
					scope = strings.TrimSpace(limit.Scope.Model.DisplayName)
				}
				label := "Weekly"
				key := "weekly_scoped"
				if scope != "" {
					label = "Weekly · " + scope
					key = "weekly_" + strings.ToLower(strings.ReplaceAll(scope, " ", "_"))
				}
				out = append(out, Window{Key: key, Label: label, UsedPercent: clampPercent(*limit.Percent), ResetsAt: limit.ResetsAt, WindowSeconds: weekSeconds, Scope: scope, Severity: limit.Severity})
			default:
				if limit.Group == "weekly" {
					out = append(out, Window{Key: limit.Kind, Label: "Weekly · " + limit.Kind, UsedPercent: clampPercent(*limit.Percent), ResetsAt: limit.ResetsAt, WindowSeconds: weekSeconds, Severity: limit.Severity})
				}
			}
		}
		return out
	}
	if usage.SevenDay != nil && usage.SevenDay.Utilization != nil {
		out = append(out, Window{Key: "weekly", Label: "Weekly", UsedPercent: clampPercent(*usage.SevenDay.Utilization), ResetsAt: usage.SevenDay.ResetsAt, WindowSeconds: weekSeconds})
	}
	if usage.SevenDayOpus != nil && usage.SevenDayOpus.Utilization != nil {
		out = append(out, Window{Key: "weekly_opus", Label: "Weekly · Opus", UsedPercent: clampPercent(*usage.SevenDayOpus.Utilization), ResetsAt: usage.SevenDayOpus.ResetsAt, WindowSeconds: weekSeconds, Scope: "Opus"})
	}
	if usage.SevenDaySonnet != nil && usage.SevenDaySonnet.Utilization != nil {
		out = append(out, Window{Key: "weekly_sonnet", Label: "Weekly · Sonnet", UsedPercent: clampPercent(*usage.SevenDaySonnet.Utilization), ResetsAt: usage.SevenDaySonnet.ResetsAt, WindowSeconds: weekSeconds, Scope: "Sonnet"})
	}
	return out
}

// organizationDisplay shortens Anthropic's auto-named personal orgs.
func organizationDisplay(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasSuffix(name, "'s Organization") || strings.HasSuffix(name, "’s Organization") {
		return "Personal"
	}
	return name
}

func claudeLabel(org, email string) string {
	switch {
	case org != "" && email != "":
		return org + " · " + email
	case org != "":
		return org
	default:
		return email
	}
}

// --- Codex (ChatGPT backend usage) ----------------------------------------

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	ResetAt            *float64 `json:"reset_at"`
	LimitWindowSeconds *float64 `json:"limit_window_seconds"`
}

type codexUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		PrimaryWindow   *codexWindow `json:"primary_window"`
		SecondaryWindow *codexWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	ResetCredits struct {
		AvailableCount *float64 `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
}

func (m *Manager) fetchCodex(ctx context.Context, cfg Config, storage map[string]any, acct *Account) error {
	token := stringField(storage, "access_token")
	if token == "" {
		return fmt.Errorf("credential has no access token")
	}
	if email := stringField(storage, "email"); email != "" {
		acct.Email = email
	}
	if acct.Label == "" || acct.Label == acct.Email {
		acct.Label = acct.Email
	}
	headers := http.Header{"Authorization": {"Bearer " + token}}
	if accountID := stringField(storage, "account_id"); accountID != "" {
		headers.Set("ChatGPT-Account-Id", accountID)
	}
	body, err := m.getJSON(ctx, cfg, codexUsageURL, headers, "codex usage")
	if err != nil {
		return err
	}
	var usage codexUsage
	if json.Unmarshal(body, &usage) != nil {
		return fmt.Errorf("codex usage response is not valid JSON")
	}
	acct.Plan = strings.TrimSpace(usage.PlanType)
	acct.Windows = codexWindows(usage)
	if usage.ResetCredits.AvailableCount != nil {
		acct.Metrics = append(acct.Metrics, pluginapi.QuotaMetric{Key: "reset_credits", Label: "Reset credits", Value: *usage.ResetCredits.AvailableCount, Unit: "credits"})
	}
	if len(acct.Windows) == 0 {
		return fmt.Errorf("codex usage response carried no rate limits")
	}
	return nil
}

func codexWindows(usage codexUsage) []Window {
	out := make([]Window, 0, 2)
	seen := map[string]bool{}
	for _, w := range []*codexWindow{usage.RateLimit.PrimaryWindow, usage.RateLimit.SecondaryWindow} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		seconds := int64(0)
		if w.LimitWindowSeconds != nil {
			seconds = int64(*w.LimitWindowSeconds)
		}
		if seconds < 0 {
			continue
		}
		key, label := "session", "5 hours"
		if seconds >= 518400 {
			key, label = "weekly", "Weekly"
		} else if seconds > 0 && seconds != sessionSeconds {
			label = fmt.Sprintf("%d hours", seconds/3600)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		win := Window{Key: key, Label: label, UsedPercent: clampPercent(*w.UsedPercent), WindowSeconds: seconds}
		if w.ResetAt != nil && *w.ResetAt > 0 {
			win.ResetsAt = time.Unix(int64(*w.ResetAt), 0).UTC().Format(time.RFC3339)
		}
		out = append(out, win)
	}
	return out
}

// --- Neuralwatt --------------------------------------------------------------

func (m *Manager) fetchNeuralwattAccount(ctx context.Context, cfg Config, storage map[string]any, acct *Account) error {
	key := stringField(storage, "api_key")
	if key == "" {
		return fmt.Errorf("credential has no api key")
	}
	if acct.Label == "" {
		acct.Label = cfg.Label
	}
	q, err := fetchNeuralwattQuota(ctx, m.host, cfg.BaseURL, key, cfg.Timeout())
	if err != nil {
		return err
	}
	now := time.Now()
	acct.Plan = strings.TrimSpace(strings.Join(nonEmpty(q.Subscription.Plan, q.Subscription.BillingInterval), " · "))
	if q.Key.Name != "" {
		acct.Email = q.Key.Name
	}
	if usedPct, ok := energyUsedPercent(q); ok {
		win := Window{Key: "energy", Label: "Energy", UsedPercent: usedPct}
		if reset, seconds, ok := energyWindow(q, now); ok {
			win.ResetsAt = reset.UTC().Format(time.RFC3339)
			win.WindowSeconds = seconds
		}
		if q.Subscription.InOverage {
			win.Severity = "overage"
		}
		acct.Windows = append(acct.Windows, win)
	}
	acct.Metrics = append(acct.Metrics, neuralwattQuotaResponse(q, now).Summary...)
	return nil
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
