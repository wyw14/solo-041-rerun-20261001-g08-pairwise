package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/intake"
	"github.com/wyw14/cry-97/internal/model"
)

// SiteView exposes the live field state the planner validates against.
// It is implemented by process.Plant.
type SiteView interface {
	LineSnapshot(id model.LineID) (model.LineState, bool)
	ActiveBatch(lineID model.LineID) (model.Batch, bool)
	IntakeCapacity(lineID model.LineID) (float64, bool)
	EqualizerSnapshot() []intake.Equalizer
}

// Starter launches a batch on a process line; implemented by process.Plant.
type Starter interface {
	StartBatch(ctx context.Context, lineID model.LineID, flow float64, source string) (model.Batch, error)
}

// Revision carries the fields an operator may adjust before a plan starts.
// Nil fields are left unchanged.
type Revision struct {
	Source      *string
	FlowM3Hour  *float64
	TargetStart *time.Time
}

func (r Revision) empty() bool {
	return r.Source == nil && r.FlowM3Hour == nil && r.TargetStart == nil
}

// SequenceEntry is one waiting plan placed into the executable order.
type SequenceEntry struct {
	PlanID      uuid.UUID    `json:"plan_id"`
	LineID      model.LineID `json:"line_id"`
	Source      string       `json:"source"`
	FlowM3Hour  float64      `json:"flow_m3_hour"`
	TargetStart time.Time    `json:"target_start"`
	Order       int          `json:"order"`
	Ready       bool         `json:"ready"`
	Detail      string       `json:"detail,omitempty"`
}

// Snapshot is the operator-facing board: plans grouped by lifecycle state,
// the executable order for waiting plans, and current equalizer occupancy.
type Snapshot struct {
	Waiting    []PlannedBatch     `json:"waiting"`
	Running    []PlannedBatch     `json:"running"`
	Blocked    []PlannedBatch     `json:"blocked"`
	Completed  []PlannedBatch     `json:"completed"`
	Cancelled  []PlannedBatch     `json:"cancelled"`
	Sequence   []SequenceEntry    `json:"sequence"`
	Equalizers []intake.Equalizer `json:"equalizers"`
	ObservedAt time.Time          `json:"observed_at"`
}

// Board holds the cross-line production plan. It never mutates started
// batches; execution re-validates live field state on every attempt.
type Board struct {
	mu    sync.Mutex
	now   func() time.Time
	site  SiteView
	plans map[uuid.UUID]PlannedBatch
}

func NewBoard(site SiteView, now func() time.Time) (*Board, error) {
	if site == nil || now == nil {
		return nil, errors.New("site view and clock are required")
	}
	return &Board{site: site, now: now, plans: make(map[uuid.UUID]PlannedBatch)}, nil
}

// Schedule registers a new pending batch for a process line.
func (b *Board) Schedule(lineID model.LineID, source string, flow float64, targetStart time.Time) (PlannedBatch, error) {
	planned, err := NewPlannedBatch(lineID, source, flow, targetStart, b.now())
	if err != nil {
		return PlannedBatch{}, err
	}
	if _, ok := b.site.LineSnapshot(lineID); !ok {
		return PlannedBatch{}, fmt.Errorf("process line %s is not found", lineID)
	}
	b.mu.Lock()
	b.plans[planned.ID] = planned
	b.mu.Unlock()
	return planned, nil
}

// Revise adjusts a pending plan. Plans that already started are immutable.
func (b *Board) Revise(id uuid.UUID, revision Revision) (PlannedBatch, error) {
	if revision.empty() {
		return PlannedBatch{}, errors.New("at least one revision field is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	planned, ok := b.plans[id]
	if !ok {
		return PlannedBatch{}, errors.New("planned batch is not found")
	}
	if err := planned.adjustable(); err != nil {
		return PlannedBatch{}, err
	}
	if revision.Source != nil {
		source := strings.TrimSpace(*revision.Source)
		if source == "" {
			return PlannedBatch{}, errors.New("intake source is required")
		}
		planned.Source = source
	}
	if revision.FlowM3Hour != nil {
		if *revision.FlowM3Hour <= 0 {
			return PlannedBatch{}, errors.New("planned flow must be positive")
		}
		planned.FlowM3Hour = *revision.FlowM3Hour
	}
	if revision.TargetStart != nil {
		if revision.TargetStart.IsZero() {
			return PlannedBatch{}, errors.New("target start time is required")
		}
		planned.TargetStart = revision.TargetStart.UTC()
	}
	planned.Status = StatusWaiting
	planned.Reason = ""
	planned.Revision++
	planned.UpdatedAt = b.now().UTC()
	b.plans[id] = planned
	return planned, nil
}

// Cancel drops a pending plan. Plans that already started are immutable.
func (b *Board) Cancel(id uuid.UUID) (PlannedBatch, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	planned, ok := b.plans[id]
	if !ok {
		return PlannedBatch{}, errors.New("planned batch is not found")
	}
	if err := planned.adjustable(); err != nil {
		return PlannedBatch{}, err
	}
	planned.Status = StatusCancelled
	planned.Reason = ""
	planned.UpdatedAt = b.now().UTC()
	b.plans[id] = planned
	return planned, nil
}

// Execute starts a pending plan after re-validating live field state.
// A failed validation marks the plan blocked with the reason recorded.
func (b *Board) Execute(ctx context.Context, id uuid.UUID, starter Starter) (PlannedBatch, model.Batch, error) {
	if starter == nil {
		return PlannedBatch{}, model.Batch{}, errors.New("batch starter is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	planned, ok := b.plans[id]
	if !ok {
		return PlannedBatch{}, model.Batch{}, errors.New("planned batch is not found")
	}
	if planned.Status != StatusWaiting && planned.Status != StatusBlocked {
		return PlannedBatch{}, model.Batch{}, errors.New("planned batch has already started")
	}
	if reason := b.fieldReason(planned); reason != "" {
		return b.blockLocked(planned, reason), model.Batch{}, errors.New(reason)
	}
	if reason := equalizerReason(b.site.EqualizerSnapshot(), planned.LineID, planned.FlowM3Hour, nil); reason != "" {
		return b.blockLocked(planned, reason), model.Batch{}, errors.New(reason)
	}
	batch, err := starter.StartBatch(ctx, planned.LineID, planned.FlowM3Hour, planned.Source)
	if err != nil {
		return b.blockLocked(planned, err.Error()), model.Batch{}, err
	}
	now := b.now().UTC()
	planned.Status = StatusRunning
	planned.Reason = ""
	planned.BatchID = batch.ID
	planned.StartedAt = now
	planned.UpdatedAt = now
	b.plans[id] = planned
	return planned, batch, nil
}

// Snapshot reconciles running plans against the field and returns the board.
func (b *Board) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reconcileLocked()
	snapshot := Snapshot{
		Waiting: []PlannedBatch{}, Running: []PlannedBatch{}, Blocked: []PlannedBatch{},
		Completed: []PlannedBatch{}, Cancelled: []PlannedBatch{},
		Equalizers: b.site.EqualizerSnapshot(), ObservedAt: b.now().UTC(),
	}
	for _, planned := range b.plans {
		switch planned.Status {
		case StatusWaiting:
			snapshot.Waiting = append(snapshot.Waiting, planned)
		case StatusRunning:
			snapshot.Running = append(snapshot.Running, planned)
		case StatusBlocked:
			snapshot.Blocked = append(snapshot.Blocked, planned)
		case StatusCompleted:
			snapshot.Completed = append(snapshot.Completed, planned)
		case StatusCancelled:
			snapshot.Cancelled = append(snapshot.Cancelled, planned)
		}
	}
	sortPlans(snapshot.Waiting)
	sortPlans(snapshot.Running)
	sortPlans(snapshot.Blocked)
	sortRecent(snapshot.Completed)
	sortRecent(snapshot.Cancelled)
	snapshot.Sequence = b.sequenceLocked()
	return snapshot
}

// blockLocked records why a plan cannot start and returns the updated plan.
func (b *Board) blockLocked(planned PlannedBatch, reason string) PlannedBatch {
	planned.Status = StatusBlocked
	planned.Reason = reason
	planned.UpdatedAt = b.now().UTC()
	b.plans[planned.ID] = planned
	return planned
}

// reconcileLocked marks running plans completed once their batch leaves the line.
func (b *Board) reconcileLocked() {
	now := b.now().UTC()
	for id, planned := range b.plans {
		if planned.Status != StatusRunning {
			continue
		}
		active, found := b.site.ActiveBatch(planned.LineID)
		if found && active.ID == planned.BatchID && active.IsActive() {
			continue
		}
		planned.Status = StatusCompleted
		planned.CompletedAt = now
		planned.UpdatedAt = now
		b.plans[id] = planned
	}
}

// sequenceLocked orders waiting plans by target time and evaluates each
// against live field state, reserving equalizer capacity as it goes so
// later entries see what earlier ones consume.
func (b *Board) sequenceLocked() []SequenceEntry {
	now := b.now().UTC()
	waiting := make([]PlannedBatch, 0, len(b.plans))
	for _, planned := range b.plans {
		if planned.Status == StatusWaiting {
			waiting = append(waiting, planned)
		}
	}
	sortPlans(waiting)
	basins := b.site.EqualizerSnapshot()
	reserved := make(map[string]bool)
	busyLines := make(map[model.LineID]bool)
	entries := make([]SequenceEntry, 0, len(waiting))
	for index, planned := range waiting {
		entry := SequenceEntry{
			PlanID: planned.ID, LineID: planned.LineID, Source: planned.Source,
			FlowM3Hour: planned.FlowM3Hour, TargetStart: planned.TargetStart, Order: index + 1,
		}
		switch {
		case now.Before(planned.TargetStart):
			entry.Detail = "未到目标处理时间"
		default:
			if reason := b.fieldReason(planned); reason != "" {
				entry.Detail = reason
			} else if busyLines[planned.LineID] {
				entry.Detail = "处理线被前序计划占用，排队等待"
			} else if reason := equalizerReason(basins, planned.LineID, planned.FlowM3Hour, reserved); reason != "" {
				entry.Detail = reason
			} else {
				entry.Ready = true
				entry.Detail = "可启动"
				busyLines[planned.LineID] = true
				reserveEqualizer(basins, planned.LineID, planned.FlowM3Hour, reserved)
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// fieldReason re-checks the process line and intake capacity for a plan.
// It returns "" when the field state allows the plan to start.
func (b *Board) fieldReason(planned PlannedBatch) string {
	line, ok := b.site.LineSnapshot(planned.LineID)
	if !ok {
		return fmt.Sprintf("处理线 %s 不存在", planned.LineID)
	}
	if line.Emergency {
		return fmt.Sprintf("处理线 %s 处于急停隔离状态", planned.LineID)
	}
	if active, found := b.site.ActiveBatch(planned.LineID); found && active.IsActive() && active.ID != planned.BatchID {
		return fmt.Sprintf("处理线 %s 已有运行中的批次", planned.LineID)
	}
	capacity, configured := b.site.IntakeCapacity(planned.LineID)
	if !configured {
		return fmt.Sprintf("处理线 %s 进水能力未配置", planned.LineID)
	}
	if planned.FlowM3Hour > capacity {
		return fmt.Sprintf("预期流量 %.0f m³/h 超出处理线 %s 进水能力 %.0f m³/h", planned.FlowM3Hour, planned.LineID, capacity)
	}
	return ""
}

// equalizerReason reports why no equalization basin can take the flow,
// skipping basins already reserved by earlier plans in the sequence.
// A basin assigned to the plan's own line is reusable, mirroring
// EqualizationPool.Assign.
func equalizerReason(basins []intake.Equalizer, lineID model.LineID, flow float64, reserved map[string]bool) string {
	fitsSomewhere := false
	for _, basin := range basins {
		if flow > basin.Capacity {
			continue
		}
		fitsSomewhere = true
		if (basin.LineID == "" || basin.LineID == lineID) && !reserved[basin.ID] {
			return ""
		}
	}
	if !fitsSomewhere {
		return fmt.Sprintf("预期流量 %.0f m³/h 超出均衡池单池容量", flow)
	}
	return fmt.Sprintf("均衡池可用容量不足，无法接纳 %.0f m³/h", flow)
}

// reserveEqualizer commits the basin a start would take: the one already
// assigned to this line, or the smallest free basin that fits the flow.
func reserveEqualizer(basins []intake.Equalizer, lineID model.LineID, flow float64, reserved map[string]bool) {
	best := ""
	var bestCapacity float64
	for _, basin := range basins {
		if reserved[basin.ID] || flow > basin.Capacity {
			continue
		}
		if basin.LineID == lineID {
			reserved[basin.ID] = true
			return
		}
		if basin.LineID == "" && (best == "" || basin.Capacity < bestCapacity) {
			best, bestCapacity = basin.ID, basin.Capacity
		}
	}
	if best != "" {
		reserved[best] = true
	}
}

// sortPlans orders plans by target time, then creation, then identity.
func sortPlans(plans []PlannedBatch) {
	sort.Slice(plans, func(i, j int) bool {
		if !plans[i].TargetStart.Equal(plans[j].TargetStart) {
			return plans[i].TargetStart.Before(plans[j].TargetStart)
		}
		if !plans[i].CreatedAt.Equal(plans[j].CreatedAt) {
			return plans[i].CreatedAt.Before(plans[j].CreatedAt)
		}
		return plans[i].ID.String() < plans[j].ID.String()
	})
}

// sortRecent orders finished plans newest first.
func sortRecent(plans []PlannedBatch) {
	sort.Slice(plans, func(i, j int) bool { return plans[i].UpdatedAt.After(plans[j].UpdatedAt) })
}
