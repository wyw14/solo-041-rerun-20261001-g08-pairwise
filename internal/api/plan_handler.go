package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/plan"
)

type schedulePlanRequest struct {
	LineID      model.LineID `json:"line_id"`
	Source      string       `json:"source"`
	FlowM3Hour  float64      `json:"flow_m3_hour"`
	TargetStart time.Time    `json:"target_start"`
}

type revisePlanRequest struct {
	Source      *string    `json:"source"`
	FlowM3Hour  *float64   `json:"flow_m3_hour"`
	TargetStart *time.Time `json:"target_start"`
}

func (s *Server) planSnapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.plant.PlanBoard().Snapshot())
}

func (s *Server) schedulePlan(w http.ResponseWriter, r *http.Request) {
	var input schedulePlanRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	lineID, err := model.ParseLineID(string(input.LineID))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	planned, err := s.plant.PlanBoard().Schedule(lineID, input.Source, input.FlowM3Hour, input.TargetStart)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, planned)
}

func (s *Server) revisePlan(w http.ResponseWriter, r *http.Request) {
	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input revisePlanRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	planned, err := s.plant.PlanBoard().Revise(planID, plan.Revision{
		Source: input.Source, FlowM3Hour: input.FlowM3Hour, TargetStart: input.TargetStart,
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, planned)
}

func (s *Server) cancelPlan(w http.ResponseWriter, r *http.Request) {
	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	planned, err := s.plant.PlanBoard().Cancel(planID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, planned)
}

func (s *Server) executePlan(w http.ResponseWriter, r *http.Request) {
	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	planned, batch, err := s.plant.PlanBoard().Execute(r.Context(), planID, s.plant)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "plan": planned})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": planned, "batch": batch})
}
