package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
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
	// Allow the LLM synthesiser to spend more time when re-classifying
	// a backlog — Tier 3 fires per row and adds ~1-2s each.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	q := r.URL.Query()
	retryReview := q.Get("retry_review") == "true"
	scope := q.Get("scope") // "" | "pending" | "review" | "all"
	if scope == "" {
		// Backwards-compat: ?retry_review=true used to be the only widening flag.
		switch {
		case retryReview:
			scope = "review"
		default:
			scope = "pending"
		}
	}

	var (
		report classifier.ClassifyReport
		err    error
	)
	switch scope {
	case "pending":
		report, err = s.classifier.ClassifyPending(ctx)
	case "review":
		report, err = s.classifier.ReclassifyPendingAndReview(ctx)
	case "all":
		// All non-terminal rows where the human hasn't edited
		// confirmed_*. Use after a classifier upgrade to refresh
		// auto-confirmed rows that may now be wrong.
		report, err = s.classifier.ReclassifyAllUnconfirmed(ctx)
	default:
		writeErr(w, http.StatusBadRequest, "invalid scope", "scope must be one of: pending, review, all")
		return
	}
	if err != nil {
		s.log.Error("classify failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "classify failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleFoldAccountsSync is the admin-gated handler for
// POST /admin/fold/accounts/sync. It mirrors the user's fold-side
// asset registry (credit cards + bank accounts) into fold_accounts.
//
// Cheap: ~3 GETs to fold's API, the user has at most a handful of
// cards and bank accounts. 30-second deadline is generous.
func (s *Server) handleFoldAccountsSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	report, err := s.foldAccountsSyncer.Sync(ctx)
	if err != nil {
		s.log.Error("fold accounts sync failed", "err", err)
		writeErr(w, http.StatusBadGateway, "fold accounts sync failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleFoldSync is the admin-gated handler for POST /admin/fold/sync.
// Pulls fold transactions into staged_fold_txns (idempotent on
// fold_uuid). Stage-only: this endpoint never classifies, never pushes
// to firefly.
//
// Two modes:
//
//   - default ("recent"): pull the most recent ?limit=N transactions
//     (default 50, max 100). Cheap; suited to the steady-state cron.
//   - "since_firefly": walk fold backwards from "now" until we cross
//     the cutoff date already mirrored in firefly_txns, capped at
//     ?max=N (default 2000). The self-healing primitive — re-running
//     it after any outage catches up automatically. Use it both for
//     manual bulk catch-up and as the cron's fold-sync step once
//     you're hands-off.
func (s *Server) handleFoldSync(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("mode")
	if mode == "" {
		mode = "recent"
	}

	switch mode {
	case "recent":
		limit := 50
		if v := q.Get("limit"); v != "" {
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
	case "since_firefly":
		// 2000 is comfortably above a year of typical activity at
		// observed densities (~150 txns/month), and the call returns
		// in seconds even at that ceiling. A bigger value ought to
		// be a deliberate operator choice.
		maxTotal := 2000
		if v := q.Get("max"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 10000 {
				writeErr(w, http.StatusBadRequest, "invalid max", "must be 1..10000")
				return
			}
			maxTotal = n
		}
		// 5-minute deadline is overkill for the fold side (sub-second
		// even at 2000 txns) but leaves headroom for SQLite contention.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		report, err := s.foldSyncer.SyncSinceFirefly(ctx, maxTotal)
		if err != nil {
			s.log.Error("fold sync since firefly failed", "err", err)
			writeErr(w, http.StatusBadGateway, "fold sync since firefly failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, report)
	default:
		writeErr(w, http.StatusBadRequest, "invalid mode", "must be one of: recent, since_firefly")
	}
}
