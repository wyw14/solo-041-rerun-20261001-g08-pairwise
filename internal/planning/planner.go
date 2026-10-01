package planning

import (
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Plan statuses are derived from live field state; they are never persisted.
const (
	StatusWaiting   = "waiting"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusCanceled  = "canceled"
)

// Reason codes explain why a due plan cannot start right now.
const (
	ReasonBeforeTargetTime  = "before_target_time"
	ReasonLineActiveBatch   = "line_active_batch"
	ReasonLineEmergency     = "line_emergency"
	ReasonLineReserved      = "line_reserved_by_plan"
	ReasonIntakeCapacity    = "intake_capacity"
	ReasonEqualizerCapacity = "equalizer_capacity"
	ReasonEqualizerReserved = "equalizer_reserved_by_plan"
	// ReasonFieldStateChanged records an execution-time race: the plan looked
	// feasible during evaluation but StartBatch rejected the live state.
	ReasonFieldStateChanged = "field_state_changed"
)

// LineView is the per-line state the scheduler reasons about.
type LineView struct {
	ID            model.LineID       `json:"id"`
	Stage         model.ProcessStage `json:"stage"`
	Emergency     bool               `json:"emergency"`
	ActiveBatchID string             `json:"active_batch_id,omitempty"`
}

// EqualizerView mirrors the current equalization pool occupancy.
type EqualizerView struct {
	ID       string       `json:"id"`
	Capacity float64      `json:"capacity"`
	LineID   model.LineID `json:"line_id,omitempty"`
}

// FieldView is the live site snapshot assembled by the process layer.
type FieldView struct {
	Now            time.Time
	Lines          []LineView
	Equalizers     []EqualizerView
	IntakeCapacity map[model.LineID]float64
}

// EntryView is a plan enriched with its computed execution status.
type EntryView struct {
	Plan
	Status             string       `json:"status"`
	Due                bool         `json:"due"`
	Executable         bool         `json:"executable"`
	Reasons            []string     `json:"reasons,omitempty"`
	CandidateLine      model.LineID `json:"candidate_line,omitempty"`
	CandidateEqualizer string       `json:"candidate_equalizer,omitempty"`
}

// OrderEntry places a due plan in the recommended execution sequence.
type OrderEntry struct {
	EntryView
	Position int `json:"position"`
}

// Board is the full planning picture returned to callers.
type Board struct {
	Now             time.Time       `json:"now"`
	Entries         []EntryView     `json:"entries"`
	ExecutableOrder []OrderEntry    `json:"executable_order"`
	Lines           []LineView      `json:"lines"`
	Equalizers      []EqualizerView `json:"equalizers"`
}

type simulation struct {
	lineByID       map[model.LineID]LineView
	takenLines     map[model.LineID]struct{}
	takenEqualizer map[string]struct{}
}

// Evaluate derives every plan's status and the executable order. The order is a
// greedy simulation in target-time order: each due plan reserves the first
// feasible line and equalizer, so later entries see earlier ones as occupied.
func Evaluate(plans []Plan, field FieldView) Board {
	now := field.Now.UTC()
	sim := &simulation{
		lineByID:       make(map[model.LineID]LineView, len(field.Lines)),
		takenLines:     make(map[model.LineID]struct{}),
		takenEqualizer: make(map[string]struct{}),
	}
	for _, line := range field.Lines {
		sim.lineByID[line.ID] = line
	}
	entries := make([]EntryView, 0, len(plans))
	indexByID := make(map[uuid.UUID]int, len(plans))
	for _, plan := range plans {
		entries = append(entries, EntryView{Plan: plan, Status: statusFor(plan, sim.lineByID)})
		indexByID[plan.ID] = len(entries) - 1
	}

	due := make([]Plan, 0)
	for _, plan := range plans {
		if plan.Canceled || plan.HasStarted() {
			continue
		}
		if now.Before(plan.TargetAt) {
			entries[indexByID[plan.ID]].Reasons = []string{ReasonBeforeTargetTime}
			continue
		}
		due = append(due, plan)
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].TargetAt.Equal(due[j].TargetAt) {
			return due[i].TargetAt.Before(due[j].TargetAt)
		}
		return due[i].CreatedAt.Before(due[j].CreatedAt)
	})

	order := make([]OrderEntry, 0, len(due))
	for position, plan := range due {
		lineID, equalizerID, reasons := sim.fit(plan, field)
		entry := entries[indexByID[plan.ID]]
		entry.Due = true
		entry.Reasons = reasons
		if lineID != "" {
			entry.Executable = true
			entry.CandidateLine = lineID
			entry.CandidateEqualizer = equalizerID
			sim.takenLines[lineID] = struct{}{}
			sim.takenEqualizer[equalizerID] = struct{}{}
		}
		entries[indexByID[plan.ID]] = entry
		order = append(order, OrderEntry{EntryView: entry, Position: position + 1})
	}

	lines := append([]LineView(nil), field.Lines...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].ID < lines[j].ID })
	equalizers := append([]EqualizerView(nil), field.Equalizers...)
	sort.Slice(equalizers, func(i, j int) bool { return equalizers[i].ID < equalizers[j].ID })
	return Board{Now: now, Entries: entries, ExecutableOrder: order, Lines: lines, Equalizers: equalizers}
}

func statusFor(plan Plan, lineByID map[model.LineID]LineView) string {
	switch {
	case plan.Canceled:
		return StatusCanceled
	case plan.HasStarted():
		if plan.BatchID != nil {
			line, ok := lineByID[plan.LineID]
			if ok && !line.Emergency && line.ActiveBatchID == plan.BatchID.String() {
				return StatusRunning
			}
		}
		return StatusCompleted
	default:
		return StatusWaiting
	}
}

// candidateOrder puts the preferred line first, then all known lines by ID.
func (s *simulation) candidateOrder(prefer model.LineID) []model.LineID {
	ids := make([]model.LineID, 0, len(s.lineByID))
	for id := range s.lineByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if prefer != "" {
		if _, ok := s.lineByID[prefer]; ok {
			ordered := make([]model.LineID, 0, len(ids)+1)
			ordered = append(ordered, prefer)
			for _, id := range ids {
				if id != prefer {
					ordered = append(ordered, id)
				}
			}
			return ordered
		}
	}
	return ids
}

// fit returns the first feasible line and equalizer for the plan, or the
// distinct reasons every candidate line is blocked.
func (s *simulation) fit(plan Plan, field FieldView) (model.LineID, string, []string) {
	chosenLine := model.LineID("")
	chosenEqualizer := ""
	reasons := []string{}
	seen := map[string]struct{}{}
	addReason := func(reason string) {
		if _, ok := seen[reason]; ok {
			return
		}
		seen[reason] = struct{}{}
		reasons = append(reasons, reason)
	}
	for _, lineID := range s.candidateOrder(plan.PreferLine) {
		line := s.lineByID[lineID]
		if line.Emergency {
			addReason(ReasonLineEmergency)
			continue
		}
		if line.ActiveBatchID != "" {
			addReason(ReasonLineActiveBatch)
			continue
		}
		if _, reserved := s.takenLines[lineID]; reserved {
			addReason(ReasonLineReserved)
			continue
		}
		capacity, configured := field.IntakeCapacity[lineID]
		if !configured || plan.FlowM3Hour > capacity {
			addReason(ReasonIntakeCapacity)
			continue
		}
		equalizerID, reason := s.fitEqualizer(plan.FlowM3Hour, field)
		if equalizerID == "" {
			addReason(reason)
			continue
		}
		chosenLine = lineID
		chosenEqualizer = equalizerID
		break
	}
	if chosenLine == "" {
		return "", "", reasons
	}
	return chosenLine, chosenEqualizer, nil
}

// fitEqualizer mirrors intake.EqualizationPool.Assign: equalizers are probed by
// ID and the first free one with enough capacity wins.
func (s *simulation) fitEqualizer(flow float64, field FieldView) (string, string) {
	ids := make([]string, 0, len(field.Equalizers))
	byID := make(map[string]EqualizerView, len(field.Equalizers))
	for _, basin := range field.Equalizers {
		ids = append(ids, basin.ID)
		byID[basin.ID] = basin
	}
	sort.Strings(ids)
	for _, id := range ids {
		basin := byID[id]
		if basin.LineID != "" {
			continue
		}
		if _, reserved := s.takenEqualizer[id]; reserved {
			continue
		}
		if flow <= basin.Capacity {
			return id, ""
		}
	}
	// Distinguish "no basin is large enough at all" from "large basins exist but
	// earlier plans in the order hold them".
	for _, basin := range field.Equalizers {
		if basin.LineID == "" && flow <= basin.Capacity {
			if _, reserved := s.takenEqualizer[basin.ID]; reserved {
				return "", ReasonEqualizerReserved
			}
		}
	}
	return "", ReasonEqualizerCapacity
}
