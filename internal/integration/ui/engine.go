package ui

// engine.go — the suggestion engine's status and its shadow evaluation.
//
//	GET  /api/engine        what the engine is doing (classifier.Status)
//	GET  /api/engine/eval   the current or last evaluation, row by row
//	POST /api/engine/eval   start one: {"limit", "offset", "model", "reasoning", "uuids", "concurrency"}
//
// The evaluation re-suggests transactions already sent to firefly, with the
// answer hidden, and grades each field against what firefly holds. It writes
// nothing. It runs in the background — a request would time out long before
// thirty model calls do — so POST answers at once and GET is polled.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
)

func (h *Handler) handleEngine(w http.ResponseWriter, r *http.Request) {
	if h.cls == nil {
		writeJSON(w, http.StatusOK, classifier.EngineStatus{Feedback: map[string]int{}})
		return
	}
	writeJSON(w, http.StatusOK, h.cls.Status(r.Context()))
}

// handlePlaces offers names for who was paid while someone types one
// (classifier.SuggestPlaces). Always 200 with a list — empty when there is
// nothing to offer — so the picker never has an error to show for a hint.
func (h *Handler) handlePlaces(w http.ResponseWriter, r *http.Request) {
	empty := classifier.PlacesResult{Suggestions: []classifier.PlaceSuggestion{}}
	if h.cls == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	res, err := h.cls.SuggestPlaces(r.Context(), r.PathValue("fold_uuid"), r.URL.Query().Get("q"))
	switch {
	case errors.Is(err, classifier.ErrNoPlaces), r.Context().Err() != nil:
		writeJSON(w, http.StatusOK, empty)
		return
	case err != nil:
		h.log.Warn("place suggestions", "fold_uuid", r.PathValue("fold_uuid"), "err", err)
		writeJSON(w, http.StatusOK, empty)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=600")
	writeJSON(w, http.StatusOK, res)
}

// handleComparePlaces: POST {"cases":[{"uuid","q"}], "models":[…]} — each
// model's suggested places for each case, with how long it took. Writes
// nothing; for choosing TEXAS_FOLDEM_LLM_FAST_MODEL on evidence.
func (h *Handler) handleComparePlaces(w http.ResponseWriter, r *http.Request) {
	if h.cls == nil {
		h.apiError(w, http.StatusServiceUnavailable, "no classifier configured")
		return
	}
	var body struct {
		Cases  []classifier.PlacesCase `json:"cases"`
		Models []string                `json:"models"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		h.apiError(w, http.StatusBadRequest, "bad request body")
		return
	}
	runs, err := h.cls.ComparePlaces(r.Context(), body.Cases, body.Models)
	if err != nil {
		h.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (h *Handler) handleEvalStatus(w http.ResponseWriter, r *http.Request) {
	if h.cls == nil {
		h.apiError(w, http.StatusServiceUnavailable, "no classifier configured")
		return
	}
	writeJSON(w, http.StatusOK, h.cls.EvalStatus())
}

func (h *Handler) handleEvalStart(w http.ResponseWriter, r *http.Request) {
	if h.cls == nil {
		h.apiError(w, http.StatusServiceUnavailable, "no classifier configured")
		return
	}
	var opt classifier.EvalOptions
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&opt); err != nil {
		h.apiError(w, http.StatusBadRequest, "bad request body")
		return
	}
	rep, err := h.cls.StartEval(opt)
	switch {
	case errors.Is(err, classifier.ErrEvalRunning):
		h.apiError(w, http.StatusConflict, err.Error())
	case err != nil:
		h.apiError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, rep)
	}
}
