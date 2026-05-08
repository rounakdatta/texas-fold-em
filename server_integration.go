package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
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

// handlePush is the admin-gated handler for
// POST /admin/push/{fold_uuid}?confirm=true.
//
// Without ?confirm=true, returns the firefly POST body that WOULD be
// sent (preview/dry-run). With ?confirm=true, performs the dedup check
// (GET /api/v1/search/transactions?query=external_id_is:<uuid>) and
// then POSTs to /api/v1/transactions if no existing match. Either way
// audit_log gets a row.
//
// Path-parameterised because the UI links to one transaction at a time;
// no batch push (yet).
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	foldUUID := r.PathValue("fold_uuid")
	if foldUUID == "" {
		writeErr(w, http.StatusBadRequest, "missing fold_uuid path parameter", "")
		return
	}
	confirm := r.URL.Query().Get("confirm") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	report, err := s.pusher.Push(ctx, foldUUID, confirm)
	if err != nil {
		switch {
		case errors.Is(err, integration.PushNotFoundError):
			writeErr(w, http.StatusNotFound, "staged transaction not found", err.Error())
		case errors.Is(err, integration.PushNotReadyError):
			writeErr(w, http.StatusConflict, "row not in pushable status", err.Error())
		case errors.Is(err, integration.PushReadOnlyError):
			writeErr(w, http.StatusServiceUnavailable, "firefly write surface is read-only", err.Error())
		default:
			s.log.Error("push failed", "fold_uuid", foldUUID, "err", err)
			writeErr(w, http.StatusBadGateway, "push failed", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleClassify is the admin-gated handler for POST /admin/classify.
// Walks every status='pending' staged fold transaction and applies the
// deterministic classifier tiers. Status transitions:
//
//	pending → ready_to_push  (high-confidence Tier-1 or Tier-2)
//	pending → needs_review   (low-confidence or Tier-4)
//
// Idempotent: rows already past 'pending' are not touched. Re-running
// classify is the recovery path when the merchant_lookup is freshly
// rebuilt — a pending row that previously fell through to needs_review
// can move to ready_to_push when the lookup learns the merchant.
//
// (For freshly-confirmed merchants to start auto-classifying, the
// caller should /admin/firefly/sync first to rebuild merchant_lookup,
// THEN /admin/classify. PR H will wire that into a single periodic
// goroutine.)
func (s *Server) handleClassify(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	report, err := s.classifier.ClassifyPending(ctx)
	if err != nil {
		s.log.Error("classify failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "classify failed", err.Error())
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
