package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultURL                    = "http://127.0.0.1:8317"
	defaultAPIVersion             = "auto"
	defaultProviders              = "claude,codex"
	defaultInterval               = 5 * time.Minute
	defaultHTTPTimeout            = 30 * time.Second
	defaultPriorityMax            = 100
	defaultPriorityStep           = 1
	defaultFiveHourMode           = "off"
	defaultFiveHourMaxUtilization = 90.0
)

type Config struct {
	URL                    string
	ManagementKey          string
	APIVersion             string
	Providers              map[string]bool
	Interval               time.Duration
	HTTPTimeout            time.Duration
	PriorityMax            int
	PriorityStep           int
	FiveHourMode           string
	FiveHourMaxUtilization float64
	DryRun                 bool
	Once                   bool
}

func Load(args []string) (Config, error) {
	cfg := Config{
		URL:                    envString("CLIPROXY_URL", defaultURL),
		ManagementKey:          firstNonEmpty(os.Getenv("CLIPROXY_MANAGEMENT_KEY"), os.Getenv("MANAGEMENT_KEY")),
		APIVersion:             strings.ToLower(envString("CLIPROXY_API_VERSION", defaultAPIVersion)),
		Interval:               envDuration("CLIPROXY_INTERVAL", defaultInterval),
		HTTPTimeout:            envDuration("CLIPROXY_HTTP_TIMEOUT", defaultHTTPTimeout),
		PriorityMax:            envInt("CLIPROXY_PRIORITY_MAX", defaultPriorityMax),
		PriorityStep:           envInt("CLIPROXY_PRIORITY_STEP", defaultPriorityStep),
		FiveHourMode:           strings.ToLower(envString("CLIPROXY_FIVE_HOUR_MODE", defaultFiveHourMode)),
		FiveHourMaxUtilization: envFloat("CLIPROXY_FIVE_HOUR_MAX_UTILIZATION", defaultFiveHourMaxUtilization),
		DryRun:                 envBool("CLIPROXY_DRY_RUN", false),
		Once:                   envBool("CLIPROXY_ONCE", false),
	}

	providersRaw := envString("CLIPROXY_PROVIDERS", defaultProviders)
	fs := flag.NewFlagSet("cliproxy-priority-controller", flag.ContinueOnError)
	fs.StringVar(&cfg.URL, "url", cfg.URL, "CLIProxyAPI base URL")
	fs.StringVar(&cfg.ManagementKey, "management-key", cfg.ManagementKey, "CLIProxyAPI Management API key")
	fs.StringVar(&cfg.APIVersion, "api-version", cfg.APIVersion, "Management API version: auto, v8, or v0")
	fs.StringVar(&providersRaw, "providers", providersRaw, "comma-separated providers to manage")
	fs.DurationVar(&cfg.Interval, "interval", cfg.Interval, "reconciliation interval")
	fs.DurationVar(&cfg.HTTPTimeout, "http-timeout", cfg.HTTPTimeout, "per-request HTTP timeout")
	fs.IntVar(&cfg.PriorityMax, "priority-max", cfg.PriorityMax, "priority assigned to the highest-ranked credential")
	fs.IntVar(&cfg.PriorityStep, "priority-step", cfg.PriorityStep, "priority decrement per rank tier")
	fs.StringVar(&cfg.FiveHourMode, "five-hour-mode", cfg.FiveHourMode, "5-hour policy: off or guardrail")
	fs.Float64Var(&cfg.FiveHourMaxUtilization, "five-hour-max-utilization", cfg.FiveHourMaxUtilization, "guardrail threshold as percent used")
	fs.BoolVar(&cfg.DryRun, "dry-run", cfg.DryRun, "show decisions without applying changes")
	fs.BoolVar(&cfg.Once, "once", cfg.Once, "run one reconciliation and exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	cfg.APIVersion = strings.ToLower(strings.TrimSpace(cfg.APIVersion))
	cfg.FiveHourMode = strings.ToLower(strings.TrimSpace(cfg.FiveHourMode))
	cfg.Providers = parseProviders(providersRaw)

	if cfg.URL == "" {
		return Config{}, errors.New("CLIProxyAPI URL cannot be empty")
	}
	switch cfg.APIVersion {
	case "auto", "v8", "v0":
	default:
		return Config{}, fmt.Errorf("invalid API version %q: expected auto, v8, or v0", cfg.APIVersion)
	}
	if len(cfg.Providers) == 0 {
		return Config{}, errors.New("at least one provider must be enabled")
	}
	for provider := range cfg.Providers {
		if provider != "claude" && provider != "codex" {
			return Config{}, fmt.Errorf("unsupported provider %q", provider)
		}
	}
	if cfg.Interval <= 0 {
		return Config{}, errors.New("interval must be greater than zero")
	}
	if cfg.HTTPTimeout <= 0 {
		return Config{}, errors.New("HTTP timeout must be greater than zero")
	}
	if cfg.PriorityStep <= 0 {
		return Config{}, errors.New("priority-step must be greater than zero")
	}
	switch cfg.FiveHourMode {
	case "off", "guardrail":
	default:
		return Config{}, fmt.Errorf("invalid five-hour-mode %q: expected off or guardrail", cfg.FiveHourMode)
	}
	if cfg.FiveHourMaxUtilization <= 0 || cfg.FiveHourMaxUtilization > 100 {
		return Config{}, errors.New("five-hour-max-utilization must be greater than 0 and at most 100")
	}
	return cfg, nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseProviders(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		provider := strings.ToLower(strings.TrimSpace(item))
		if provider != "" {
			out[provider] = true
		}
	}
	return out
}
