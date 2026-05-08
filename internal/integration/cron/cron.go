// Package cron runs the periodic fold→classify pipeline. Lives in
// its own subpackage so the integration package itself doesn't depend
// on the classifier subpackage (which would create a cycle since the
// classifier's tests import integration to set up the SQLite schema).
package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
)

// PeriodicSync runs FoldSyncer.SyncRecent then ClassifyPending on a
// fixed cadence. Designed to run as a goroutine launched from main:
//
//	go cron.PeriodicSync(ctx, fs, cls, 1*time.Hour, 50, log)
//
// Returns when ctx is cancelled.
//
// Failure mode: any error in fold sync or classify is logged but
// doesn't stop the loop. The cadence is the recovery — if fold's API
// is briefly down, the next tick retries.
func PeriodicSync(
	ctx context.Context,
	foldSyncer *integration.FoldSyncer,
	cls *classifier.Classifier,
	every time.Duration,
	limit int,
	log *slog.Logger,
) {
	if every <= 0 {
		log.Info("periodic sync disabled (interval <= 0)")
		return
	}
	if limit <= 0 {
		limit = 50
	}
	log = log.With("component", "periodic_sync")
	log.Info("starting periodic sync loop", "interval", every, "limit", limit)

	// Tick immediately on startup so an operator who restarts the pod
	// gets a fresh sync without waiting `every`. Then back to the
	// regular cadence.
	tick := time.NewTimer(0)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("periodic sync stopping", "reason", ctx.Err())
			return
		case <-tick.C:
			runOneCycle(ctx, foldSyncer, cls, limit, log)
			tick.Reset(every)
		}
	}
}

func runOneCycle(
	ctx context.Context,
	foldSyncer *integration.FoldSyncer,
	cls *classifier.Classifier,
	limit int,
	log *slog.Logger,
) {
	if foldSyncer != nil {
		report, err := foldSyncer.SyncRecent(ctx, limit)
		if err != nil {
			log.Warn("fold sync error (cycle continues)", "err", err)
			return
		}
		log.Info("periodic fold sync",
			"fetched", report.Fetched,
			"inserted", report.Inserted,
			"skipped", report.Skipped,
		)
	}
	if cls != nil {
		report, err := cls.ClassifyPending(ctx)
		if err != nil {
			log.Warn("classify error (cycle continues)", "err", err)
			return
		}
		log.Info("periodic classify",
			"examined", report.Examined,
			"auto", report.AutoClassified,
			"review", report.NeedsReview,
		)
	}
}
