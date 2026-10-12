package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lucas-varjao/cliproxy-priority-controller/internal/cliproxy"
)

type Result struct {
	ResetAt     time.Time
	Utilization float64
	Source      string
}

type UpstreamError struct {
	Provider   string
	StatusCode int
	Body       string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s quota endpoint returned HTTP %d: %s", e.Provider, e.StatusCode, strings.TrimSpace(e.Body))
}

func IsUnauthorized(err error) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusUnauthorized
}

func Fetch(ctx context.Context, client *cliproxy.Client, credential cliproxy.Credential, userAgent string, now time.Time) (Result, error) {
	switch credential.ProviderName() {
	case "claude":
		return fetchClaude(ctx, client, credential, userAgent)
	case "codex":
		return fetchCodex(ctx, client, credential, userAgent, now)
	default:
		return Result{}, fmt.Errorf("unsupported provider %q", credential.ProviderName())
	}
}

func fetchClaude(ctx context.Context, client *cliproxy.Client, credential cliproxy.Credential, userAgent string) (Result, error) {
	response, err := client.APICall(ctx, cliproxy.APICallRequest{
		AuthIndex: credential.AuthIndex,
		Method:    http.MethodGet,
		URL:       "https://api.anthropic.com/api/oauth/usage",
		Header: map[string]string{
			"Authorization":  "Bearer $TOKEN$",
			"Accept":         "application/json",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
			"User-Agent":     userAgent,
		},
	})
	if err != nil {
		return Result{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, &UpstreamError{Provider: "claude", StatusCode: response.StatusCode, Body: response.Body}
	}
	return parseClaude([]byte(response.Body))
}

func parseClaude(data []byte) (Result, error) {
	var payload struct {
		SevenDay *struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		} `json:"seven_day"`
		Limits []struct {
			Kind     string  `json:"kind"`
			Group    string  `json:"group"`
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resets_at"`
			IsActive bool    `json:"is_active"`
			Scope    struct {
				Model any `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Result{}, fmt.Errorf("parse Claude quota response: %w", err)
	}
	if payload.SevenDay != nil && strings.TrimSpace(payload.SevenDay.ResetsAt) != "" {
		reset, err := parseTime(payload.SevenDay.ResetsAt)
		if err != nil {
			return Result{}, fmt.Errorf("parse Claude seven_day reset: %w", err)
		}
		return Result{ResetAt: reset, Utilization: payload.SevenDay.Utilization, Source: "seven_day"}, nil
	}
	for _, limit := range payload.Limits {
		kind := strings.ToLower(strings.TrimSpace(limit.Kind))
		if kind != "weekly_all" && kind != "weekly" && !strings.Contains(kind, "weekly") {
			continue
		}
		if strings.TrimSpace(limit.ResetsAt) == "" {
			continue
		}
		reset, err := parseTime(limit.ResetsAt)
		if err != nil {
			continue
		}
		return Result{ResetAt: reset, Utilization: limit.Percent, Source: "limits:" + kind}, nil
	}
	return Result{}, errors.New("Claude response did not contain a weekly reset")
}

func fetchCodex(ctx context.Context, client *cliproxy.Client, credential cliproxy.Credential, userAgent string, now time.Time) (Result, error) {
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    userAgent,
	}
	if accountID := credential.CodexAccountID(); accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	response, err := client.APICall(ctx, cliproxy.APICallRequest{
		AuthIndex: credential.AuthIndex,
		Method:    http.MethodGet,
		URL:       "https://chatgpt.com/backend-api/wham/usage",
		Header:    headers,
	})
	if err != nil {
		return Result{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, &UpstreamError{Provider: "codex", StatusCode: response.StatusCode, Body: response.Body}
	}
	return parseCodex([]byte(response.Body), now)
}

type codexWindow struct {
	UsedPercent       float64
	WindowSeconds     int64
	ResetAfterSeconds int64
	ResetAt           time.Time
	Name              string
}

func parseCodex(data []byte, now time.Time) (Result, error) {
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return Result{}, fmt.Errorf("parse Codex quota response: %w", err)
	}

	rateLimit, _ := payload["rate_limit"].(map[string]any)
	if rateLimit == nil {
		return Result{}, errors.New("Codex response did not contain rate_limit")
	}

	var candidates []codexWindow
	for _, name := range []string{"primary_window", "secondary_window"} {
		if raw, ok := rateLimit[name].(map[string]any); ok {
			if window, ok := parseCodexWindow(raw, name, now); ok {
				candidates = append(candidates, window)
			}
		}
	}

	for _, window := range candidates {
		if window.WindowSeconds >= 6*24*60*60 && window.WindowSeconds < 28*24*60*60 {
			return Result{ResetAt: window.ResetAt, Utilization: window.UsedPercent, Source: "rate_limit:" + window.Name}, nil
		}
	}
	for _, window := range candidates {
		if window.Name == "secondary_window" {
			return Result{ResetAt: window.ResetAt, Utilization: window.UsedPercent, Source: "rate_limit:secondary_window"}, nil
		}
	}
	for _, window := range candidates {
		if window.WindowSeconds == 0 && window.Name == "primary_window" {
			return Result{ResetAt: window.ResetAt, Utilization: window.UsedPercent, Source: "rate_limit:primary_window-legacy"}, nil
		}
	}

	return Result{}, errors.New("Codex response did not contain a weekly reset window")
}

func parseCodexWindow(raw map[string]any, name string, now time.Time) (codexWindow, bool) {
	window := codexWindow{
		UsedPercent:       number(raw["used_percent"]),
		WindowSeconds:     int64(number(first(raw, "limit_window_seconds", "window_seconds"))),
		ResetAfterSeconds: int64(number(raw["reset_after_seconds"])),
		Name:              name,
	}

	if resetAt := number(raw["reset_at"]); resetAt > 0 {
		seconds := int64(resetAt)
		if seconds > 1_000_000_000_000 {
			window.ResetAt = time.UnixMilli(seconds).UTC()
		} else {
			window.ResetAt = time.Unix(seconds, 0).UTC()
		}
	} else if rawReset, ok := raw["resets_at"].(string); ok && strings.TrimSpace(rawReset) != "" {
		parsed, err := parseTime(rawReset)
		if err == nil {
			window.ResetAt = parsed
		}
	} else if window.ResetAfterSeconds > 0 {
		window.ResetAt = now.Add(time.Duration(window.ResetAfterSeconds) * time.Second).UTC()
	}
	return window, !window.ResetAt.IsZero()
}

func parseTime(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", raw)
}

func first(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func number(value any) float64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed
	default:
		return 0
	}
}
