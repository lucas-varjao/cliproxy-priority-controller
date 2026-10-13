package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lucas-varjao/cliproxy-priority-controller/internal/cliproxy"
	"github.com/lucas-varjao/cliproxy-priority-controller/internal/config"
	"github.com/lucas-varjao/cliproxy-priority-controller/internal/controller"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	userAgent := "cliproxy-priority-controller/" + version
	client := cliproxy.New(cfg.URL, cfg.ManagementKey, cfg.APIVersion, userAgent, cfg.HTTPTimeout)
	ctrl := controller.New(client, controller.Config{
		Providers:              cfg.Providers,
		PriorityMax:            cfg.PriorityMax,
		PriorityStep:           cfg.PriorityStep,
		DryRun:                 cfg.DryRun,
		UserAgent:              userAgent,
		FiveHourMode:           cfg.FiveHourMode,
		FiveHourMaxUtilization: cfg.FiveHourMaxUtilization,
		QuotaRequestSpacing:    cfg.QuotaRequestSpacing,
	}, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apiVersion, err := client.APIVersion(ctx)
	if err != nil {
		return err
	}
	logger.Info("controller started",
		"version", version,
		"api_version", apiVersion,
		"dry_run", cfg.DryRun,
		"once", cfg.Once,
		"interval", cfg.Interval,
		"five_hour_mode", cfg.FiveHourMode,
		"five_hour_max_utilization", cfg.FiveHourMaxUtilization,
		"quota_request_spacing", cfg.QuotaRequestSpacing,
	)

	if err := ctrl.Reconcile(ctx); err != nil {
		logger.Error("reconciliation completed with errors", "error", err)
		if cfg.Once {
			return err
		}
	}
	if cfg.Once {
		return nil
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("controller stopped")
			return nil
		case <-ticker.C:
			if err := ctrl.Reconcile(ctx); err != nil {
				logger.Error("reconciliation completed with errors", "error", err)
			}
		}
	}
}
