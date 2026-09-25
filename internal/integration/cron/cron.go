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

// PeriodicSync runs FoldSyncer.SyncSinceFirefly then ClassifyPending
// on a fixed cadence. Designed to run as a goroutine launched from
// main:
//
//	go cron.PeriodicSync(ctx, fs, cls, 1*time.Hour, 2000, log)
//
// Returns when ctx is cancelled.
//
// Self-healing by design: each tick fills the gap between firefly's
// most recent date and now, capped at maxTotal. If a tick is missed
// (broker outage, fold-API hiccup, machine reboot), the next tick
// catches up automatically — no operator intervention needed. The
// per-cycle cap is what prevents a runaway after a long outage; tune
// it up only if you genuinely need to backfill more than a year of
// activity in a single tick.
//
// Failure mode: any error in fold sync or classify is logged but
// doesn't stop the loop. The cadence is the recovery.
func PeriodicSync(
	ctx context.Context,
	foldSyncer *integration.FoldSyncer,
	foldAccountsSyncer *integration.FoldAccountsSyncer,
	fireflyAccountsSyncer *integration.FireflyAccountsSyncer,
	cls *classifier.Classifier,
	every time.Duration,
	maxTotal int,
	log *slog.Logger,
) {
	if every <= 0 {
		log.Info("periodic sync disabled (interval <= 0)")
		return
	}
	if maxTotal <= 0 {
		maxTotal = 2000
	}
	log = log.With("component", "periodic_sync")
	log.Info("starting periodic sync loop", "interval", every, "max_total", maxTotal)

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
			runOneCycle(ctx, foldSyncer, foldAccountsSyncer, fireflyAccountsSyncer, cls, maxTotal, log)
			tick.Reset(every)
		}
	}
}

func runOneCycle(
	ctx context.Context,
	foldSyncer *integration.FoldSyncer,
	foldAccountsSyncer *integration.FoldAccountsSyncer,
	fireflyAccountsSyncer *integration.FireflyAccountsSyncer,
	cls *classifier.Classifier,
	maxTotal int,
	log *slog.Logger,
) {
	// Refresh the fold-accounts mirror first so newly-used cards are known
	// before we classify — otherwise source-account grounding misses any
	// card added since the last manual sync. Cheap (a few GETs).
	if foldAccountsSyncer != nil {
		if report, err := foldAccountsSyncer.Sync(ctx); err != nil {
			log.Warn("fold accounts sync error (cycle continues)", "err", err)
		} else {
			log.Info("periodic fold accounts sync", "fetched", report.Fetched, "upserted", report.Upserted)
		}
	}
	// Refresh firefly's OWN account list too. This is what lets a firefly
	// asset the user just *created* (no transactions on it yet) be matched
	// as a transaction's source card within one tick — the gap that left
	// "Ixigo AU Bank Credit Card" unmatched. Cheap (one or two GETs).
	if fireflyAccountsSyncer != nil {
		if report, err := fireflyAccountsSyncer.Sync(ctx); err != nil {
			log.Warn("firefly accounts sync error (cycle continues)", "err", err)
		} else {
			log.Info("periodic firefly accounts sync", "fetched", report.Fetched, "assets", report.Assets)
		}
	}
	if foldSyncer != nil {
		report, err := foldSyncer.SyncSinceFirefly(ctx, maxTotal)
		if err != nil {
			log.Warn("fold sync error (cycle continues)", "err", err)
			return
		}
		log.Info("periodic fold sync",
			"fetched", report.Fetched,
			"inserted", report.Inserted,
			"skipped", report.Skipped,
			"pages", report.Pages,
			"stopped_at", report.StoppedAt,
			"cutoff_date", report.CutoffDate,
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
			"refunds", report.RefundHits,
		)
		// Point refunds at their purchases where nothing has yet — rows
		// classified before refunds were understood, hand-edited rows, and
		// manual deposits. Cheap SQL; only proposed_refund_of changes.
		if n, err := cls.MatchRefunds(ctx); err != nil {
			log.Warn("refund matching error (cycle continues)", "err", err)
		} else if n > 0 {
			log.Info("periodic refund matching", "matched", n)
		}
	}
}
