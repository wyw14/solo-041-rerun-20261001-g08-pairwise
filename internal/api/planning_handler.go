package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/planning"
	"github.com/wyw14/cry-97/internal/process"
)

type createPlanRequest struct {
	Source     string  `json:"source"`
	FlowM3Hour float64 `json:"flow_m3_hour"`
	TargetAt   string  `json:"target_at"`
	PreferLine string  `json:"prefer_line,omitempty"`
}

type adjustPlanRequest struct {
	Source     *string  `json:"source,omitempty"`
	FlowM3Hour *float64 `json:"flow_m3_hour,omitempty"`
	TargetAt   *string  `json:"target_at,omitempty"`
	PreferLine *string  `json:"prefer_line,omitempty"`
}

// parseTargetTime accepts RFC3339 timestamps and datetime-local values
// ("2026-09-30T08:00"), treating naive values as UTC.
func parseTargetTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, errors.New("target start time is required")
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("target start time must be RFC3339 or YYYY-MM-DDTHH:MM")
}

func parsePreferLine(raw string) (model.LineID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	return model.ParseLineID(raw)
}

func (s *Server) planningBoard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.plant.PlanningBoard())
}

func (s *Server) createProductionPlan(w http.ResponseWriter, r *http.Request) {
	var input createPlanRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	targetAt, err := parseTargetTime(input.TargetAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	preferLine, err := parsePreferLine(input.PreferLine)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	board, err := s.plant.CreatePlan(input.Source, input.FlowM3Hour, targetAt, preferLine)
	if err != nil {
		writePlanError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, board)
}

func (s *Server) adjustProductionPlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("production plan id is invalid"))
		return
	}
	var input adjustPlanRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	change := planning.Adjustment{
		Source:     input.Source,
		Flow:       input.FlowM3Hour,
		PreferLine: nil,
	}
	if input.TargetAt != nil {
		targetAt, parseErr := parseTargetTime(*input.TargetAt)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		change.TargetAt = &targetAt
	}
	if input.PreferLine != nil {
		preferLine, parseErr := parsePreferLine(*input.PreferLine)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		change.PreferLine = &preferLine
	}
	board, err := s.plant.AdjustPlan(id, change)
	if err != nil {
		writePlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) cancelProductionPlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("production plan id is invalid"))
		return
	}
	board, err := s.plant.CancelPlan(id)
	if err != nil {
		writePlanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) executeProductionPlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("production plan id is invalid"))
		return
	}
	batch, board, err := s.plant.ExecutePlan(r.Context(), id)
	if err != nil {
		writeExecutionError(w, err, board)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"batch": batch, "board": board})
}

func (s *Server) executeNextProductionPlan(w http.ResponseWriter, r *http.Request) {
	batch, board, err := s.plant.ExecuteNextPlan(r.Context())
	if err != nil {
		writeExecutionError(w, err, board)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"batch": batch, "board": board})
}

func writePlanError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, process.ErrPlanNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, planning.ErrPlanStarted), errors.Is(err, planning.ErrPlanCanceled):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusBadRequest, err)
	}
}

func writeExecutionError(w http.ResponseWriter, err error, board any) {
	var blocked *process.PlanBlockedError
	if errors.As(err, &blocked) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   err.Error(),
			"plan_id": blocked.PlanID,
			"reasons": blocked.Reasons,
			"board":   board,
		})
		return
	}
	switch {
	case errors.Is(err, process.ErrPlanNotFound):
		writeError(w, http.StatusNotFound, err)
	default:
		writeError(w, http.StatusConflict, err)
	}
}
