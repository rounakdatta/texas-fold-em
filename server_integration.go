package main

import (
	"context"
	"net/http"
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
