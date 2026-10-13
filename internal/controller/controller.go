package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lucas-varjao/cliproxy-priority-controller/internal/cliproxy"
	"github.com/lucas-varjao/cliproxy-priority-controller/internal/quota"
)

const (
	FiveHourModeOff       = "off"
	FiveHourModeGuardrail = "guardrail"

	quotaPollLowUtilization  = 30 * time.Minute
	quotaPollMidUtilization  = 15 * time.Minute
	quotaPollHighUtilization = 10 * time.Minute
	quotaPollAfterStaleReset = 5 * time.Minute
	quotaResetGrace          = 30 * time.Second

	rateLimitBackoffMin = 15 * time.Minute
	rateLimitBackoffMax = 2 * time.Hour
)

type Config struct {
	Providers              map[string]bool
	PriorityMax            int
	PriorityStep           int
	DryRun                 bool
	UserAgent              string
	FiveHourMode           string
	FiveHourMaxUtilization float64
	QuotaRequestSpacing    time.Duration
}

type Controller struct {
	client *cliproxy.Client
	config Config
	logger *slog.Logger
	now    func() time.Time

	quotaCache       map[string]quotaCacheEntry
	lastQuotaRequest time.Time
}

type quotaCacheEntry struct {
	Result      quota.Result
	HasResult   bool
	NextCheck   time.Time
	RateLimited bool
	Backoff     time.Duration
}

type observation struct {
	Credential cliproxy.Credential
	Quota      quota.Result
	Cached     bool
	NextCheck  time.Time
}

type Change struct {
	Provider    string
	Credential  cliproxy.Credential
	Quota       quota.Result
	Cached      bool
	NextCheck   time.Time
	Decision    string
	OldPriority int
	NewPriority int
}

type providerBackoffError struct {
	Until time.Time
}

func (e *providerBackoffError) Error() string {
	return "quota rate-limit backoff active until " + e.Until.Format(time.RFC3339)
}

func New(client *cliproxy.Client, cfg Config, logger *slog.Logger) *Controller {
	return &Controller{
		client:     client,
		config:     cfg,
		logger:     logger,
		now:        time.Now,
		quotaCache: make(map[string]quotaCacheEntry),
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	credentials, err := c.client.ListCredentials(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}

	byProvider := make(map[string][]cliproxy.Credential)
	activeCacheKeys := make(map[string]struct{})
	for _, credential := range credentials {
		provider := credential.ProviderName()
		if !c.config.Providers[provider] || credential.Disabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(credential.AccountType), "api_key") {
			c.logger.Debug("skipping API-key credential", "credential", credential.TargetName(), "provider", provider)
			continue
		}
		if strings.TrimSpace(credential.AuthIndex) == "" {
			c.logger.Debug("skipping credential without auth_index", "credential", credential.TargetName(), "provider", provider)
			continue
		}
		byProvider[provider] = append(byProvider[provider], credential)
		activeCacheKeys[credentialCacheKey(credential)] = struct{}{}
	}
	c.pruneQuotaCache(activeCacheKeys)

	var reconcileErr error
	for provider, providerCredentials := range byProvider {
		if len(providerCredentials) == 0 {
			continue
		}
		changes, err := c.planProvider(ctx, provider, providerCredentials)
		if err != nil {
			var backoffErr *providerBackoffError
			switch {
			case errors.As(err, &backoffErr):
				c.logger.Debug("provider quota polling paused", "provider", provider, "retry_at", backoffErr.Until.Format(time.RFC3339))
			case quota.IsRateLimited(err):
				c.logger.Warn("provider quota polling rate limited; priorities left unchanged", "provider", provider, "error", err)
			default:
				c.logger.Error("provider reconciliation skipped", "provider", provider, "error", err)
				reconcileErr = errorsJoin(reconcileErr, err)
			}
			continue
		}
		for _, change := range changes {
			attrs := changeLogAttrs(change)
			if c.config.DryRun {
				c.logger.Info("would update priority", attrs...)
				continue
			}
			if err := c.client.PatchPriority(ctx, change.Credential, change.NewPriority); err != nil {
				reconcileErr = errorsJoin(reconcileErr, fmt.Errorf("patch %s priority: %w", change.Credential.TargetName(), err))
				c.logger.Error("failed to update priority", append(attrs, "error", err)...)
				continue
			}
			c.logger.Info("updated priority", attrs...)
		}
	}
	return reconcileErr
}

func (c *Controller) planProvider(ctx context.Context, provider string, credentials []cliproxy.Credential) ([]Change, error) {
	if until := c.providerBackoffUntil(credentials); !until.IsZero() {
		return nil, &providerBackoffError{Until: until}
	}

	observations := make([]observation, 0, len(credentials))
	for _, credential := range credentials {
		result, cached, nextCheck, err := c.quotaForCredential(ctx, credential)
		if err != nil {
			return nil, fmt.Errorf("%s quota for %s: %w", provider, credential.TargetName(), err)
		}
		observations = append(observations, observation{
			Credential: credential,
			Quota:      result,
			Cached:     cached,
			NextCheck:  nextCheck,
		})
	}

	sortObservations(observations, c.config.FiveHourMode, c.config.FiveHourMaxUtilization)
	priorities := AssignPriorities(observations, c.config.PriorityMax, c.config.PriorityStep, c.config.FiveHourMode, c.config.FiveHourMaxUtilization)

	changes := make([]Change, 0, len(observations))
	for i, item := range observations {
		newPriority := priorities[i]
		decision := decisionFor(item, c.config.FiveHourMode, c.config.FiveHourMaxUtilization)
		if item.Credential.Priority == newPriority {
			attrs := observationLogAttrs(provider, item, decision, item.Credential.Priority, newPriority)
			if c.config.DryRun {
				c.logger.Info("priority already correct", attrs...)
			} else {
				c.logger.Debug("priority already correct", attrs...)
			}
			continue
		}
		changes = append(changes, Change{
			Provider:    provider,
			Credential:  item.Credential,
			Quota:       item.Quota,
			Cached:      item.Cached,
			NextCheck:   item.NextCheck,
			Decision:    decision,
			OldPriority: item.Credential.Priority,
			NewPriority: newPriority,
		})
	}
	return changes, nil
}

func (c *Controller) quotaForCredential(ctx context.Context, credential cliproxy.Credential) (quota.Result, bool, time.Time, error) {
	now := c.now().UTC()
	key := credentialCacheKey(credential)
	entry, exists := c.quotaCache[key]
	if exists && now.Before(entry.NextCheck) {
		if entry.RateLimited {
			return quota.Result{}, false, entry.NextCheck, &providerBackoffError{Until: entry.NextCheck}
		}
		if entry.HasResult {
			return entry.Result, true, entry.NextCheck, nil
		}
	}

	result, err := c.fetchQuotaWithRefresh(ctx, credential)
	if err != nil {
		if quota.IsRateLimited(err) {
			backoff := nextRateLimitBackoff(entry.Backoff)
			wait := backoff
			if retryAfter := quota.RateLimitRetryAfter(err); retryAfter > wait {
				wait = retryAfter
			}
			entry.RateLimited = true
			entry.Backoff = backoff
			entry.NextCheck = now.Add(wait)
			c.quotaCache[key] = entry
			c.logger.Warn("quota endpoint rate limited; backing off",
				"provider", credential.ProviderName(),
				"credential", credential.TargetName(),
				"retry_at", entry.NextCheck.Format(time.RFC3339),
				"backoff", wait,
			)
		}
		return quota.Result{}, false, time.Time{}, err
	}

	nextCheck := nextQuotaCheck(now, result, c.config.FiveHourMode, c.config.FiveHourMaxUtilization)
	c.quotaCache[key] = quotaCacheEntry{
		Result:    result,
		HasResult: true,
		NextCheck: nextCheck,
	}
	return result, false, nextCheck, nil
}

func (c *Controller) fetchQuotaWithRefresh(ctx context.Context, credential cliproxy.Credential) (quota.Result, error) {
	result, err := c.fetchQuota(ctx, credential)
	if err == nil || !quota.IsUnauthorized(err) {
		return result, err
	}

	c.logger.Info("quota token unauthorized; refreshing credential once",
		"provider", credential.ProviderName(),
		"credential", credential.TargetName(),
	)
	if refreshErr := c.client.RefreshCredential(ctx, credential); refreshErr != nil {
		return quota.Result{}, fmt.Errorf("refresh credential after quota 401: %w", refreshErr)
	}
	return c.fetchQuota(ctx, credential)
}

func (c *Controller) fetchQuota(ctx context.Context, credential cliproxy.Credential) (quota.Result, error) {
	if err := c.waitForQuotaRequestSpacing(ctx); err != nil {
		return quota.Result{}, err
	}
	c.lastQuotaRequest = time.Now()
	return quota.Fetch(ctx, c.client, credential, c.config.UserAgent, c.now().UTC())
}

func (c *Controller) waitForQuotaRequestSpacing(ctx context.Context) error {
	if c.config.QuotaRequestSpacing <= 0 || c.lastQuotaRequest.IsZero() {
		return nil
	}
	wait := c.config.QuotaRequestSpacing - time.Since(c.lastQuotaRequest)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Controller) providerBackoffUntil(credentials []cliproxy.Credential) time.Time {
	now := c.now().UTC()
	var latest time.Time
	for _, credential := range credentials {
		entry, ok := c.quotaCache[credentialCacheKey(credential)]
		if !ok || !entry.RateLimited || !now.Before(entry.NextCheck) {
			continue
		}
		if entry.NextCheck.After(latest) {
			latest = entry.NextCheck
		}
	}
	return latest
}

func (c *Controller) pruneQuotaCache(active map[string]struct{}) {
	for key := range c.quotaCache {
		if _, ok := active[key]; !ok {
			delete(c.quotaCache, key)
		}
	}
}

func credentialCacheKey(credential cliproxy.Credential) string {
	identity := strings.TrimSpace(credential.AuthIndex)
	if identity == "" {
		identity = strings.TrimSpace(credential.ID)
	}
	if identity == "" {
		identity = credential.TargetName()
	}
	return credential.ProviderName() + "\x00" + identity
}

func nextQuotaCheck(now time.Time, result quota.Result, fiveHourMode string, maxUtilization float64) time.Time {
	interval := quotaPollLowUtilization
	if fiveHourMode == FiveHourModeGuardrail && result.FiveHour != nil {
		utilization := result.FiveHour.Utilization
		midThreshold := max(0, maxUtilization-20)
		highThreshold := max(0, maxUtilization-5)
		switch {
		case utilization >= maxUtilization:
			if result.FiveHour.ResetAt.After(now) {
				next := result.FiveHour.ResetAt.Add(quotaResetGrace)
				return earlierResetCheck(now, next, result.Weekly.ResetAt)
			}
			interval = quotaPollAfterStaleReset
		case utilization >= highThreshold:
			interval = quotaPollHighUtilization
		case utilization >= midThreshold:
			interval = quotaPollMidUtilization
		}
	}

	next := now.Add(interval)
	return earlierResetCheck(now, next, result.Weekly.ResetAt)
}

func earlierResetCheck(now, current, resetAt time.Time) time.Time {
	if !resetAt.After(now) {
		return current
	}
	resetCheck := resetAt.Add(quotaResetGrace)
	if resetCheck.Before(current) {
		return resetCheck
	}
	return current
}

func nextRateLimitBackoff(previous time.Duration) time.Duration {
	if previous < rateLimitBackoffMin {
		return rateLimitBackoffMin
	}
	next := previous * 2
	if next > rateLimitBackoffMax {
		return rateLimitBackoffMax
	}
	return next
}

func sortObservations(items []observation, fiveHourMode string, maxUtilization float64) {
	sort.SliceStable(items, func(i, j int) bool {
		leftConstrained := isFiveHourConstrained(items[i], fiveHourMode, maxUtilization)
		rightConstrained := isFiveHourConstrained(items[j], fiveHourMode, maxUtilization)
		if leftConstrained != rightConstrained {
			return !leftConstrained
		}
		if items[i].Quota.Weekly.ResetAt.Equal(items[j].Quota.Weekly.ResetAt) {
			return items[i].Credential.TargetName() < items[j].Credential.TargetName()
		}
		return items[i].Quota.Weekly.ResetAt.Before(items[j].Quota.Weekly.ResetAt)
	})
}

func AssignPriorities(items []observation, maxPriority, step int, fiveHourMode string, maxUtilization float64) []int {
	priorities := make([]int, len(items))
	if len(items) == 0 {
		return priorities
	}

	priority := maxPriority
	priorities[0] = priority
	for i := 1; i < len(items); i++ {
		if !samePriorityTier(items[i-1], items[i], fiveHourMode, maxUtilization) {
			priority -= step
		}
		priorities[i] = priority
	}
	return priorities
}

func samePriorityTier(left, right observation, fiveHourMode string, maxUtilization float64) bool {
	if isFiveHourConstrained(left, fiveHourMode, maxUtilization) != isFiveHourConstrained(right, fiveHourMode, maxUtilization) {
		return false
	}
	return left.Quota.Weekly.ResetAt.Equal(right.Quota.Weekly.ResetAt)
}

func isFiveHourConstrained(item observation, fiveHourMode string, maxUtilization float64) bool {
	if fiveHourMode != FiveHourModeGuardrail || item.Quota.FiveHour == nil {
		return false
	}
	return item.Quota.FiveHour.Utilization >= maxUtilization
}

func decisionFor(item observation, fiveHourMode string, maxUtilization float64) string {
	if isFiveHourConstrained(item, fiveHourMode, maxUtilization) {
		return "five-hour-guardrail"
	}
	return "weekly-reset"
}

func changeLogAttrs(change Change) []any {
	return observationLogAttrs(change.Provider, observation{
		Credential: change.Credential,
		Quota:      change.Quota,
		Cached:     change.Cached,
		NextCheck:  change.NextCheck,
	}, change.Decision, change.OldPriority, change.NewPriority)
}

func observationLogAttrs(provider string, item observation, decision string, oldPriority, newPriority int) []any {
	attrs := []any{
		"provider", provider,
		"credential", item.Credential.TargetName(),
		"decision", decision,
		"quota_cached", item.Cached,
		"quota_next_check_at", item.NextCheck.Format(time.RFC3339),
		"weekly_reset_at", item.Quota.Weekly.ResetAt.Format(time.RFC3339),
		"weekly_utilization", item.Quota.Weekly.Utilization,
		"weekly_source", item.Quota.Weekly.Source,
	}
	if item.Quota.FiveHour != nil {
		attrs = append(attrs,
			"five_hour_reset_at", item.Quota.FiveHour.ResetAt.Format(time.RFC3339),
			"five_hour_utilization", item.Quota.FiveHour.Utilization,
			"five_hour_source", item.Quota.FiveHour.Source,
		)
	} else {
		attrs = append(attrs, "five_hour_available", false)
	}
	return append(attrs,
		"old_priority", oldPriority,
		"new_priority", newPriority,
	)
}

func errorsJoin(left, right error) error {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	return fmt.Errorf("%v; %w", left, right)
}
