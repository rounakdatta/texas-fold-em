package cron

import (
	"context"
	"log/slog"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
)

// Resuggest keeps waiting cards' suggestions current (classifier/resuggest.go):
// every `every` it revisits up to `limit` cards that a newer correction or a
// newer engine could suggest better.
func Resuggest(ctx context.Context, cls *classifier.Classifier, every time.Duration, limit int, log *slog.Logger) {
	if every <= 0 {
		log.Info("resuggest disabled (interval <= 0)")
		return
	}
	log = log.With("component", "resuggest")
	log.Info("starting resuggest loop", "interval", every, "limit", limit)
	// Let the startup sync and classify settle before the first pass.
	tick := time.NewTimer(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("resuggest stopping", "reason", ctx.Err())
			return
		case <-tick.C:
		}
		if _, err := cls.Resuggest(ctx, limit); err != nil {
			log.Warn("resuggest pass failed", "err", err)
		}
		tick.Reset(every)
	}
}
