package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/lucas-varjao/cliproxy-priority-controller/internal/cliproxy"
	"github.com/lucas-varjao/cliproxy-priority-controller/internal/quota"
)

type Config struct {
	Providers    map[string]bool
	PriorityMax  int
	PriorityStep int
	DryRun       bool
	UserAgent    string
}

type Controller struct {
	client *cliproxy.Client
	config Config
	logger *slog.Logger
	now    func() time.Time
}

type observation struct {
	Credential cliproxy.Credential
	Quota      quota.Result
}

type Change struct {
	Provider    string
	Credential  cliproxy.Credential
	ResetAt     time.Time
	Utilization float64
	OldPriority int
	NewPriority int
}

func New(client *cliproxy.Client, cfg Config, logger *slog.Logger) *Controller {
	return &Controller{client: client, config: cfg, logger: logger, now: time.Now}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	credentials, err := c.client.ListCredentials(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}

	byProvider := make(map[string][]cliproxy.Credential)
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
	}

	var reconcileErr error
	for provider, providerCredentials := range byProvider {
		if len(providerCredentials) == 0 {
			continue
		}
		changes, err := c.planProvider(ctx, provider, providerCredentials)
		if err != nil {
			c.logger.Error("provider reconciliation skipped", "provider", provider, "error", err)
			reconcileErr = errorsJoin(reconcileErr, err)
			continue
		}
		for _, change := range changes {
			attrs := []any{
				"provider", change.Provider,
				"credential", change.Credential.TargetName(),
				"reset_at", change.ResetAt.Format(time.RFC3339),
				"utilization", change.Utilization,
				"old_priority", change.OldPriority,
				"new_priority", change.NewPriority,
			}
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
	observations := make([]observation, 0, len(credentials))
	for _, credential := range credentials {
		result, err := c.fetchQuotaWithRefresh(ctx, credential)
		if err != nil {
			return nil, fmt.Errorf("%s quota for %s: %w", provider, credential.TargetName(), err)
		}
		observations = append(observations, observation{Credential: credential, Quota: result})
	}

	sort.SliceStable(observations, func(i, j int) bool {
		if observations[i].Quota.ResetAt.Equal(observations[j].Quota.ResetAt) {
			return observations[i].Credential.TargetName() < observations[j].Credential.TargetName()
		}
		return observations[i].Quota.ResetAt.Before(observations[j].Quota.ResetAt)
	})

	priorities := AssignPriorities(observations, c.config.PriorityMax, c.config.PriorityStep)
	changes := make([]Change, 0, len(observations))
	for i, item := range observations {
		newPriority := priorities[i]
		if item.Credential.Priority == newPriority {
			c.logger.Debug("priority already correct",
				"provider", provider,
				"credential", item.Credential.TargetName(),
				"priority", newPriority,
				"reset_at", item.Quota.ResetAt.Format(time.RFC3339),
			)
			continue
		}
		changes = append(changes, Change{
			Provider:    provider,
			Credential:  item.Credential,
			ResetAt:     item.Quota.ResetAt,
			Utilization: item.Quota.Utilization,
			OldPriority: item.Credential.Priority,
			NewPriority: newPriority,
		})
	}
	return changes, nil
}

func (c *Controller) fetchQuotaWithRefresh(ctx context.Context, credential cliproxy.Credential) (quota.Result, error) {
	result, err := quota.Fetch(ctx, c.client, credential, c.config.UserAgent, c.now().UTC())
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
	return quota.Fetch(ctx, c.client, credential, c.config.UserAgent, c.now().UTC())
}

func AssignPriorities(items []observation, maxPriority, step int) []int {
	priorities := make([]int, len(items))
	if len(items) == 0 {
		return priorities
	}

	priority := maxPriority
	priorities[0] = priority
	for i := 1; i < len(items); i++ {
		if !items[i].Quota.ResetAt.Equal(items[i-1].Quota.ResetAt) {
			priority -= step
		}
		priorities[i] = priority
	}
	return priorities
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
