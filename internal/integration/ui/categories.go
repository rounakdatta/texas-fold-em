package ui

// categories.go — making a category where it turns out to be missing.
//
// The deck's category picker offers the categories fold knows from Firefly. A
// new kind of spend (the first cooking-gas refill) has no category yet, and
// the moment a person notices is while picking one — so the picker offers to
// create it: fold makes it in Firefly, records it in its own copy of Firefly's
// categories, and the card takes it in the same step.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
)

type categoryReply struct {
	OK bool  `json:"ok"`
	ID int64 `json:"id"`
	// Name is the category as Firefly spells it — for a name fold already
	// knew in another case, the existing spelling.
	Name string `json:"name"`
	// Created: it is new in Firefly (false: it already existed).
	Created bool `json:"created"`
}

// handleAPICreateCategory is POST /admin/ui/api/categories {"name": "…"}.
func (h *Handler) handleAPICreateCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		h.apiError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if h.pusher == nil {
		h.apiError(w, http.StatusServiceUnavailable, "Creating categories isn’t set up on this fold.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id, name, created, err := h.pusher.CreateCategory(ctx, body.Name)
	switch {
	case errors.Is(err, integration.ErrCategoryName):
		h.apiError(w, http.StatusBadRequest, "Give the category a name — up to 100 characters.")
		return
	case errors.Is(err, integration.PushReadOnlyError):
		h.apiError(w, http.StatusForbidden, "fold is read-only right now, so nothing can be added to Firefly.")
		return
	case err != nil:
		// Firefly may already have it under that name while fold's copy
		// hasn't caught up: refresh the copy, and use it if it's there.
		if h.fireflyAccounts != nil {
			if _, serr := h.fireflyAccounts.Sync(ctx); serr == nil {
				if id, name, ok := integration.CategoryByName(ctx, h.db, body.Name); ok {
					writeJSON(w, http.StatusOK, categoryReply{OK: true, ID: id, Name: name})
					return
				}
			}
		}
		h.log.Warn("create category failed", "err", err)
		h.apiError(w, http.StatusBadGateway, "Couldn’t reach Firefly — try again in a moment.")
		return
	}
	writeJSON(w, http.StatusOK, categoryReply{OK: true, ID: id, Name: name, Created: created})
}
