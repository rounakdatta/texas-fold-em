package main

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// handleFireflySync is the admin-gated handler for
// POST /admin/firefly/sync. It walks every page of the firefly
// transactions endpoint, mirrors them into our SQLite, and rebuilds
// merchant_lookup. Read-only against firefly: the typed firefly client
// in this build cannot make non-GET calls.
//
// We pick a generous 5-minute deadline. The full sync at 7k journals,
// 50/page = ~140 pages × ~100ms ≈ 15s in practice, plus the rebuild
// CTE which is sub-second on this corpus. The 5-minute ceiling is for
// bad-network scenarios.
func (s *Server) handleFireflySync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	report, err := s.fireflySyncer.SyncAll(ctx)
	if err != nil {
		s.log.Error("firefly sync failed", "err", err)
		writeErr(w, http.StatusBadGateway, "firefly sync failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleFoldSync is the admin-gated handler for POST /admin/fold/sync.
// Pulls recent fold transactions and stages them in staged_fold_txns
// (idempotent on fold_uuid). Optional ?limit=N override; defaults to 50.
//
// Stage-only: this endpoint never classifies, never pushes to firefly.
// Those happen in subsequent admin endpoints (PR D's classifier and PR
// F's push) so each step is independently gated and auditable.
func (s *Server) handleFoldSync(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 100 {
			writeErr(w, http.StatusBadRequest, "invalid limit", "must be 1..100")
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	report, err := s.foldSyncer.SyncRecent(ctx, limit)
	if err != nil {
		s.log.Error("fold sync failed", "err", err)
		writeErr(w, http.StatusBadGateway, "fold sync failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}
