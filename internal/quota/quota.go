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

type Window struct {
	ResetAt     time.Time
	Utilization float64
	Source      string
}

type Result struct {
	FiveHour *Window
	Weekly   Window
}

type UpstreamError struct {
	Provider   string
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("%s quota endpoint returned HTTP %d: %s", e.Provider, e.StatusCode, strings.TrimSpace(e.Body))
}

func IsUnauthorized(err error) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusUnauthorized
}

func IsRateLimited(err error) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusTooManyRequests
}

func RateLimitRetryAfter(err error) time.Duration {
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	return upstream.RetryAfter
}

func Fetch(ctx context.Context, client *cliproxy.Client, credential cliproxy.Credential, userAgent string, now time.Time) (Result, error) {
	switch credential.ProviderName() {
	case "claude":
		return fetchClaude(ctx, client, credential, userAgent, now)
	case "codex":
		return fetchCodex(ctx, client, credential, userAgent, now)
	default:
		return Result{}, fmt.Errorf("unsupported provider %q", credential.ProviderName())
	}
}

func fetchClaude(ctx context.Context, client *cliproxy.Client, credential cliproxy.Credential, userAgent string, now time.Time) (Result, error) {
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
		return Result{}, newUpstreamError("claude", response, now)
	}
	return parseClaude([]byte(response.Body))
}

type claudeBucket struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
	IsActive bool    `json:"is_active"`
	Scope    struct {
		Model any `json:"model"`
	} `json:"scope"`
}

func parseClaude(data []byte) (Result, error) {
	var payload struct {
		FiveHour *claudeBucket `json:"five_hour"`
		SevenDay *claudeBucket `json:"seven_day"`
		Limits   []claudeLimit `json:"limits"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Result{}, fmt.Errorf("parse Claude quota response: %w", err)
	}

	fiveHour, err := claudeBucketWindow(payload.FiveHour, "five_hour")
	if err != nil {
		return Result{}, fmt.Errorf("parse Claude five_hour reset: %w", err)
	}
	if fiveHour == nil {
		fiveHour = findClaudeLimitWindow(payload.Limits, false)
	}

	weekly, err := claudeBucketWindow(payload.SevenDay, "seven_day")
	if err != nil {
		return Result{}, fmt.Errorf("parse Claude seven_day reset: %w", err)
	}
	if weekly == nil {
		weekly = findClaudeLimitWindow(payload.Limits, true)
	}
	if weekly == nil {
		return Result{}, errors.New("Claude response did not contain a weekly reset")
	}

	return Result{FiveHour: fiveHour, Weekly: *weekly}, nil
}

func claudeBucketWindow(bucket *claudeBucket, source string) (*Window, error) {
	if bucket == nil || strings.TrimSpace(bucket.ResetsAt) == "" {
		return nil, nil
	}
	reset, err := parseTime(bucket.ResetsAt)
	if err != nil {
		return nil, err
	}
	return &Window{ResetAt: reset, Utilization: bucket.Utilization, Source: source}, nil
}

func findClaudeLimitWindow(limits []claudeLimit, weekly bool) *Window {
	matches := func(kind string, exactOnly bool) bool {
		kind = strings.ToLower(strings.TrimSpace(kind))
		if weekly {
			if kind == "weekly_all" || kind == "weekly" {
				return true
			}
			return !exactOnly && strings.Contains(kind, "weekly")
		}
		if kind == "session" || kind == "five_hour" || kind == "five-hour" {
			return true
		}
		return !exactOnly && (strings.Contains(kind, "session") || strings.Contains(kind, "five"))
	}

	for _, exactOnly := range []bool{true, false} {
		for _, limit := range limits {
			if !matches(limit.Kind, exactOnly) || strings.TrimSpace(limit.ResetsAt) == "" {
				continue
			}
			reset, err := parseTime(limit.ResetsAt)
			if err != nil {
				continue
			}
			kind := strings.ToLower(strings.TrimSpace(limit.Kind))
			return &Window{ResetAt: reset, Utilization: limit.Percent, Source: "limits:" + kind}
		}
	}
	return nil
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
		return Result{}, newUpstreamError("codex", response, now)
	}
	return parseCodex([]byte(response.Body), now)
}

func newUpstreamError(provider string, response cliproxy.APICallResponse, now time.Time) *UpstreamError {
	return &UpstreamError{
		Provider:   provider,
		StatusCode: response.StatusCode,
		Body:       response.Body,
		RetryAfter: parseRetryAfter(response.Header, now),
	}
}

func parseRetryAfter(headers map[string][]string, now time.Time) time.Duration {
	var raw string
	for key, values := range headers {
		if !strings.EqualFold(key, "Retry-After") || len(values) == 0 {
			continue
		}
		raw = strings.TrimSpace(values[0])
		break
	}
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(raw); err == nil {
		delay := deadline.Sub(now)
		if delay > 0 {
			return delay
		}
	}
	return 0
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

	weekly := selectCodexWeekly(candidates)
	if weekly == nil {
		return Result{}, errors.New("Codex response did not contain a weekly reset window")
	}
	fiveHour := selectCodexFiveHour(candidates)

	result := Result{Weekly: codexResultWindow(*weekly)}
	if fiveHour != nil {
		window := codexResultWindow(*fiveHour)
		result.FiveHour = &window
	}
	return result, nil
}

func selectCodexWeekly(candidates []codexWindow) *codexWindow {
	for i := range candidates {
		window := &candidates[i]
		if window.WindowSeconds >= 6*24*60*60 && window.WindowSeconds < 28*24*60*60 {
			return window
		}
	}
	for i := range candidates {
		window := &candidates[i]
		if window.Name == "secondary_window" && window.WindowSeconds == 0 {
			return window
		}
	}
	if len(candidates) == 1 && candidates[0].Name == "primary_window" && candidates[0].WindowSeconds == 0 {
		return &candidates[0]
	}
	return nil
}

func selectCodexFiveHour(candidates []codexWindow) *codexWindow {
	const (
		minSessionWindow = 4 * time.Hour
		maxSessionWindow = 6 * time.Hour
	)
	for i := range candidates {
		window := &candidates[i]
		duration := time.Duration(window.WindowSeconds) * time.Second
		if duration >= minSessionWindow && duration <= maxSessionWindow {
			return window
		}
	}
	if len(candidates) >= 2 {
		for i := range candidates {
			window := &candidates[i]
			if window.Name == "primary_window" && window.WindowSeconds == 0 {
				return window
			}
		}
	}
	return nil
}

func codexResultWindow(window codexWindow) Window {
	return Window{
		ResetAt:     window.ResetAt,
		Utilization: window.UsedPercent,
		Source:      "rate_limit:" + window.Name,
	}
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
