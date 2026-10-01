package planning

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

var (
	line1 = model.LineID("line-1")
	line2 = model.LineID("line-2")
	line3 = model.LineID("line-3")
)

func testField(now time.Time) FieldView {
	return FieldView{
		Now: now,
		Lines: []LineView{
			{ID: line1, Stage: model.StageReceiving},
			{ID: line2, Stage: model.StageReceiving},
			{ID: line3, Stage: model.StageReceiving},
		},
		Equalizers: []EqualizerView{
			{ID: "EQ-line-1", Capacity: 1600},
			{ID: "EQ-line-2", Capacity: 1750},
			{ID: "EQ-line-3", Capacity: 1900},
		},
		IntakeCapacity: map[model.LineID]float64{line1: 1800, line2: 1800, line3: 1800},
	}
}

func mustPlan(t *testing.T, source string, flow float64, target time.Time, prefer model.LineID, now time.Time) Plan {
	t.Helper()
	plan, err := NewPlan(source, flow, target, prefer, now)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return plan
}

func entryByID(board Board, id uuid.UUID) EntryView {
	for _, entry := range board.Entries {
		if entry.ID == id {
			return entry
		}
	}
	return EntryView{}
}

func TestWaitingBeforeTargetTime(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	plan := mustPlan(t, "north-intake", 700, now.Add(time.Hour), "", now)
	board := Evaluate([]Plan{plan}, testField(now))

	entry := board.Entries[0]
	if entry.Status != StatusWaiting || entry.Due || entry.Executable {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if len(entry.Reasons) != 1 || entry.Reasons[0] != ReasonBeforeTargetTime {
		t.Fatalf("reasons = %v", entry.Reasons)
	}
	if len(board.ExecutableOrder) != 0 {
		t.Fatalf("future plan must not appear in order: %+v", board.ExecutableOrder)
	}
}

func TestDuePlanPicksFirstFeasibleLineAndEqualizer(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	plan := mustPlan(t, "north-intake", 700, now.Add(-time.Minute), "", now)
	board := Evaluate([]Plan{plan}, testField(now))

	entry := board.Entries[0]
	if !entry.Executable || entry.CandidateLine != line1 || entry.CandidateEqualizer != "EQ-line-1" {
		t.Fatalf("unexpected candidate: %+v", entry)
	}
	if len(board.ExecutableOrder) != 1 || board.ExecutableOrder[0].Position != 1 {
		t.Fatalf("order = %+v", board.ExecutableOrder)
	}
}

func TestActiveBatchMovesPlanToOtherLine(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	field := testField(now)
	field.Lines[0].ActiveBatchID = uuid.NewString()
	plan := mustPlan(t, "north-intake", 700, now, "", now)
	board := Evaluate([]Plan{plan}, field)

	entry := board.Entries[0]
	if !entry.Executable || entry.CandidateLine != line2 {
		t.Fatalf("expected reroute to line-2: %+v", entry)
	}
}

func TestEmergencyLinesAreSkipped(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	field := testField(now)
	field.Lines[0].Emergency = true
	field.Lines[1].Emergency = true
	plan := mustPlan(t, "north-intake", 700, now, "", now)
	board := Evaluate([]Plan{plan}, field)

	entry := board.Entries[0]
	if !entry.Executable || entry.CandidateLine != line3 {
		t.Fatalf("expected line-3: %+v", entry)
	}
}

func TestAllLinesBlockedReportsReasons(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	field := testField(now)
	for i := range field.Lines {
		field.Lines[i].ActiveBatchID = uuid.NewString()
	}
	plan := mustPlan(t, "north-intake", 700, now, "", now)
	board := Evaluate([]Plan{plan}, field)

	entry := board.Entries[0]
	if entry.Executable {
		t.Fatalf("plan must not be executable: %+v", entry)
	}
	if len(entry.Reasons) != 1 || entry.Reasons[0] != ReasonLineActiveBatch {
		t.Fatalf("reasons = %v", entry.Reasons)
	}
	if board.ExecutableOrder[0].Executable {
		t.Fatalf("blocked plan still marked executable in order")
	}
}

func TestGreedyOrderReservesLinesAndEqualizers(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	first := mustPlan(t, "north-intake", 1700, now, "", now)
	second := mustPlan(t, "south-intake", 1700, now, "", now.Add(time.Minute))
	third := mustPlan(t, "east-intake", 1700, now, "", now.Add(2*time.Minute))
	first.CreatedAt = now
	second.CreatedAt = now.Add(time.Minute)
	third.CreatedAt = now.Add(2 * time.Minute)
	board := Evaluate([]Plan{first, second, third}, testField(now))

	firstEntry := entryByID(board, first.ID)
	secondEntry := entryByID(board, second.ID)
	thirdEntry := entryByID(board, third.ID)
	if !firstEntry.Executable || firstEntry.CandidateLine != line1 || firstEntry.CandidateEqualizer != "EQ-line-2" {
		t.Fatalf("first = %+v", firstEntry)
	}
	if !secondEntry.Executable || secondEntry.CandidateLine != line2 || secondEntry.CandidateEqualizer != "EQ-line-3" {
		t.Fatalf("second = %+v", secondEntry)
	}
	if thirdEntry.Executable {
		t.Fatalf("third plan must be blocked: %+v", thirdEntry)
	}
	reasonSet := map[string]bool{}
	for _, reason := range thirdEntry.Reasons {
		reasonSet[reason] = true
	}
	if !reasonSet[ReasonLineReserved] || !reasonSet[ReasonEqualizerReserved] {
		t.Fatalf("third reasons = %v", thirdEntry.Reasons)
	}
	if board.ExecutableOrder[0].ID != first.ID || board.ExecutableOrder[2].ID != third.ID {
		t.Fatalf("order not sorted by target time: %+v", board.ExecutableOrder)
	}
}

func TestEqualizerTooSmallReportsCapacity(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	field := FieldView{
		Now: now,
		Lines: []LineView{
			{ID: line1, Stage: model.StageReceiving},
		},
		Equalizers:     []EqualizerView{{ID: "EQ-line-1", Capacity: 1600}},
		IntakeCapacity: map[model.LineID]float64{line1: 3000},
	}
	plan := mustPlan(t, "north-intake", 2000, now, "", now)
	board := Evaluate([]Plan{plan}, field)
	if got := board.Entries[0].Reasons; len(got) != 1 || got[0] != ReasonEqualizerCapacity {
		t.Fatalf("reasons = %v", got)
	}
}

func TestIntakeCapacityRejected(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	plan := mustPlan(t, "north-intake", 1950, now, "", now)
	board := Evaluate([]Plan{plan}, testField(now))
	if got := board.Entries[0].Reasons; len(got) != 1 || got[0] != ReasonIntakeCapacity {
		t.Fatalf("reasons = %v", got)
	}
}

func TestPreferredLineWinsAndFallsBack(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	plan := mustPlan(t, "north-intake", 700, now, line3, now)
	board := Evaluate([]Plan{plan}, testField(now))
	if entry := board.Entries[0]; !entry.Executable || entry.CandidateLine != line3 {
		t.Fatalf("preferred line ignored: %+v", entry)
	}

	field := testField(now)
	field.Lines[2].ActiveBatchID = uuid.NewString()
	board = Evaluate([]Plan{plan}, field)
	if entry := board.Entries[0]; !entry.Executable || entry.CandidateLine != line1 {
		t.Fatalf("expected fallback to line-1: %+v", entry)
	}
}

func TestStartedPlanStatusTracksLiveBatch(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	plan := mustPlan(t, "north-intake", 700, now, "", now)
	batchID := uuid.New()
	started := plan
	started.StartedAt = &now
	started.LineID = line1
	started.BatchID = &batchID

	field := testField(now)
	field.Lines[0].ActiveBatchID = batchID.String()
	if entry := Evaluate([]Plan{started}, field).Entries[0]; entry.Status != StatusRunning {
		t.Fatalf("expected running, got %+v", entry)
	}

	field.Lines[0].ActiveBatchID = ""
	if entry := Evaluate([]Plan{started}, field).Entries[0]; entry.Status != StatusCompleted {
		t.Fatalf("expected completed after batch closes, got %+v", entry)
	}

	field.Lines[0].ActiveBatchID = batchID.String()
	field.Lines[0].Emergency = true
	if entry := Evaluate([]Plan{started}, field).Entries[0]; entry.Status != StatusCompleted {
		t.Fatalf("expected completed after isolation, got %+v", entry)
	}
}

func TestCanceledPlanExcludedFromOrder(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	plan := mustPlan(t, "north-intake", 700, now, "", now)
	plan.Canceled = true
	board := Evaluate([]Plan{plan}, testField(now))
	if board.Entries[0].Status != StatusCanceled || len(board.ExecutableOrder) != 0 {
		t.Fatalf("canceled plan mishandled: %+v", board)
	}
}

func TestBookAdjustCancelGuards(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	book := NewBook()
	plan := mustPlan(t, "north-intake", 700, now, "", now)
	if err := book.Create(plan); err != nil {
		t.Fatalf("create: %v", err)
	}
	source := "south-intake"
	flow := 900.0
	if _, err := book.Adjust(plan.ID, Adjustment{Source: &source, Flow: &flow}, now); err != nil {
		t.Fatalf("adjust waiting plan: %v", err)
	}
	if _, err := book.Cancel(plan.ID, now); err != nil {
		t.Fatalf("cancel waiting plan: %v", err)
	}
	if _, err := book.Adjust(plan.ID, Adjustment{Source: &source}, now); err == nil {
		t.Fatal("adjust on canceled plan must fail")
	}
	if _, err := book.Cancel(plan.ID, now); err == nil {
		t.Fatal("double cancel must fail")
	}

	started := mustPlan(t, "east-intake", 500, now, "", now)
	if err := book.Create(started); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := book.MarkStarted(started.ID, line1, uuid.New(), now); err != nil {
		t.Fatalf("mark started: %v", err)
	}
	if _, err := book.Adjust(started.ID, Adjustment{Flow: &flow}, now); err == nil {
		t.Fatal("adjust on started plan must fail")
	}
	if _, err := book.Cancel(started.ID, now); err == nil {
		t.Fatal("cancel on started plan must fail")
	}
}

func TestNewPlanValidation(t *testing.T) {
	now := time.Now().UTC()
	if _, err := NewPlan("  ", 10, now, "", now); err == nil {
		t.Error("blank source accepted")
	}
	if _, err := NewPlan("north", 0, now, "", now); err == nil {
		t.Error("zero flow accepted")
	}
	if _, err := NewPlan("north", 10, time.Time{}, "", now); err == nil {
		t.Error("zero target accepted")
	}
}
