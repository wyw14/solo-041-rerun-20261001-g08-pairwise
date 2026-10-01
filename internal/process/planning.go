package process

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/planning"
)

// ErrPlanNotFound is returned when a production plan does not exist.
var ErrPlanNotFound = errors.New("production plan is not found")

// PlanBlockedError explains why a due plan could not be started at execution
// time after the live field state was re-checked.
type PlanBlockedError struct {
	PlanID  uuid.UUID
	Reasons []string
}

func (e *PlanBlockedError) Error() string {
	return "production plan cannot start with the current field state"
}

// fieldView assembles the live snapshot the scheduler reasons about. After a
// restart the in-memory batch index is empty, so the line snapshot's batch id
// is used as the occupancy fallback until the batch is clearly terminal.
func (p *Plant) fieldView() planning.FieldView {
	lines := p.Lines()
	lineViews := make([]planning.LineView, 0, len(lines))
	capacities := make(map[model.LineID]float64, len(lines))
	for _, line := range lines {
		view := planning.LineView{ID: line.ID, Stage: line.Stage, Emergency: line.Emergency}
		if batch, ok := p.ActiveBatch(line.ID); ok && batch.IsActive() {
			view.ActiveBatchID = batch.ID.String()
		} else if line.BatchID != "" && line.Stage != model.StageClosed && line.Stage != model.StageIsolated {
			view.ActiveBatchID = line.BatchID
		}
		lineViews = append(lineViews, view)
		if capacity, ok := p.intake.Capacity(line.ID); ok {
			capacities[line.ID] = capacity
		}
	}
	snapshot := p.equalizers.Snapshot()
	equalizerViews := make([]planning.EqualizerView, 0, len(snapshot))
	for _, basin := range snapshot {
		equalizerViews = append(equalizerViews, planning.EqualizerView{
			ID: basin.ID, Capacity: basin.Capacity, LineID: basin.LineID,
		})
	}
	return planning.FieldView{
		Now:            p.now(),
		Lines:          lineViews,
		Equalizers:     equalizerViews,
		IntakeCapacity: capacities,
	}
}

// PlanningBoard recomputes every plan status against current field state.
func (p *Plant) PlanningBoard() planning.Board {
	return planning.Evaluate(p.plans.List(), p.fieldView())
}

func (p *Plant) lineConfigured(lineID model.LineID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.lines[lineID]
	return ok
}

// CreatePlan registers a waiting batch plan.
func (p *Plant) CreatePlan(source string, flow float64, targetAt time.Time, preferLine model.LineID) (planning.Board, error) {
	if preferLine != "" && !p.lineConfigured(preferLine) {
		return planning.Board{}, errors.New("preferred process line is not configured")
	}
	plan, err := planning.NewPlan(source, flow, targetAt, preferLine, p.now())
	if err != nil {
		return planning.Board{}, err
	}
	if err := p.plans.Create(plan); err != nil {
		return planning.Board{}, err
	}
	if err := p.planStore.Save(p.plans.List()); err != nil {
		return planning.Board{}, err
	}
	return p.PlanningBoard(), nil
}

// AdjustPlan changes a waiting plan. Started and canceled plans are rejected.
func (p *Plant) AdjustPlan(id uuid.UUID, change planning.Adjustment) (planning.Board, error) {
	if change.PreferLine != nil && *change.PreferLine != "" && !p.lineConfigured(*change.PreferLine) {
		return planning.Board{}, errors.New("preferred process line is not configured")
	}
	if _, err := p.plans.Adjust(id, change, p.now()); err != nil {
		return planning.Board{}, err
	}
	if err := p.planStore.Save(p.plans.List()); err != nil {
		return planning.Board{}, err
	}
	return p.PlanningBoard(), nil
}

// CancelPlan withdraws a waiting plan; started batches are never touched.
func (p *Plant) CancelPlan(id uuid.UUID) (planning.Board, error) {
	if _, err := p.plans.Cancel(id, p.now()); err != nil {
		return planning.Board{}, err
	}
	if err := p.planStore.Save(p.plans.List()); err != nil {
		return planning.Board{}, err
	}
	return p.PlanningBoard(), nil
}

func boardEntry(board planning.Board, id uuid.UUID) (planning.EntryView, bool) {
	for _, entry := range board.Entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return planning.EntryView{}, false
}

// ExecutePlan re-validates field state at start time and, if the candidate line
// is still feasible, starts the real batch through the existing intake path.
func (p *Plant) ExecutePlan(ctx context.Context, id uuid.UUID) (model.Batch, planning.Board, error) {
	plan, ok := p.plans.Get(id)
	if !ok {
		return model.Batch{}, planning.Board{}, ErrPlanNotFound
	}
	now := p.now()
	if plan.Canceled {
		return model.Batch{}, planning.Board{}, errors.New("plan is canceled and cannot start")
	}
	if plan.HasStarted() {
		return model.Batch{}, planning.Board{}, errors.New("plan has already started")
	}
	board := planning.Evaluate(p.plans.List(), p.fieldView())
	entry, found := boardEntry(board, id)
	if !found {
		return model.Batch{}, planning.Board{}, ErrPlanNotFound
	}
	if !entry.Executable {
		reasons := entry.Reasons
		if len(reasons) == 0 {
			reasons = []string{planning.ReasonBeforeTargetTime}
		}
		_, _ = p.plans.Reject(id, reasons, now)
		_ = p.planStore.Save(p.plans.List())
		board = p.PlanningBoard()
		if entry, found = boardEntry(board, id); found {
			reasons = entry.Reasons
		}
		return model.Batch{}, board, &PlanBlockedError{PlanID: id, Reasons: reasons}
	}

	// StartBatch re-checks intake configuration, equalizer availability and the
	// active batch guard against the very latest state.
	batch, err := p.StartBatch(ctx, entry.CandidateLine, plan.FlowM3Hour, plan.Source)
	if err != nil {
		reasons := []string{planning.ReasonFieldStateChanged}
		_, _ = p.plans.Reject(id, reasons, now)
		_ = p.planStore.Save(p.plans.List())
		board = p.PlanningBoard()
		return model.Batch{}, board, &PlanBlockedError{PlanID: id, Reasons: reasons}
	}
	if _, err := p.plans.MarkStarted(id, entry.CandidateLine, batch.ID, p.now()); err != nil {
		return model.Batch{}, planning.Board{}, err
	}
	if err := p.planStore.Save(p.plans.List()); err != nil {
		return model.Batch{}, planning.Board{}, err
	}
	return batch, p.PlanningBoard(), nil
}

// ExecuteNextPlan starts the first executable plan in target-time order.
func (p *Plant) ExecuteNextPlan(ctx context.Context) (model.Batch, planning.Board, error) {
	board := p.PlanningBoard()
	for _, order := range board.ExecutableOrder {
		if !order.Executable {
			continue
		}
		return p.ExecutePlan(ctx, order.ID)
	}
	if len(board.ExecutableOrder) > 0 {
		head := board.ExecutableOrder[0]
		return model.Batch{}, board, &PlanBlockedError{PlanID: head.ID, Reasons: head.Reasons}
	}
	return model.Batch{}, board, errors.New("no due production plan is waiting")
}
