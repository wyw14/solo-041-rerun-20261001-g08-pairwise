package planning

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Sentinel errors allow API layers to map immutable-plan attempts to 409.
var (
	ErrPlanStarted  = errors.New("plan has already started and cannot be changed")
	ErrPlanCanceled = errors.New("plan is canceled and cannot be changed")
)

// Rejection records why an execution attempt could not start a batch.
type Rejection struct {
	At      time.Time `json:"at"`
	Reasons []string  `json:"reasons"`
}

// Plan is a scheduled, not yet started batch request. Once started it is
// immutable: adjustments and cancellation no longer touch the live batch.
type Plan struct {
	ID         uuid.UUID    `json:"id"`
	Source     string       `json:"source"`
	FlowM3Hour float64      `json:"flow_m3_hour"`
	TargetAt   time.Time    `json:"target_at"`
	PreferLine model.LineID `json:"prefer_line,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	LineID     model.LineID `json:"line_id,omitempty"`
	BatchID    *uuid.UUID   `json:"batch_id,omitempty"`
	Canceled   bool         `json:"canceled"`
	LastReject *Rejection   `json:"last_reject,omitempty"`
}

// NewPlan validates the request and normalizes timestamps to UTC.
func NewPlan(source string, flow float64, target time.Time, preferLine model.LineID, now time.Time) (Plan, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Plan{}, errors.New("influent source is required")
	}
	if flow <= 0 {
		return Plan{}, errors.New("expected flow must be positive")
	}
	if target.IsZero() {
		return Plan{}, errors.New("target start time is required")
	}
	stamp := now.UTC()
	return Plan{
		ID:         uuid.New(),
		Source:     source,
		FlowM3Hour: flow,
		TargetAt:   target.UTC(),
		PreferLine: preferLine,
		CreatedAt:  stamp,
		UpdatedAt:  stamp,
	}, nil
}

// HasStarted reports whether the plan already produced a live batch.
func (p Plan) HasStarted() bool {
	return p.StartedAt != nil || p.BatchID != nil
}

// Adjustment carries the optional fields that may change before start.
type Adjustment struct {
	Source     *string
	Flow       *float64
	TargetAt   *time.Time
	PreferLine *model.LineID
}

func (p Plan) apply(change Adjustment, now time.Time) (Plan, error) {
	if change.Source != nil {
		source := strings.TrimSpace(*change.Source)
		if source == "" {
			return Plan{}, errors.New("influent source is required")
		}
		p.Source = source
	}
	if change.Flow != nil {
		if *change.Flow <= 0 {
			return Plan{}, errors.New("expected flow must be positive")
		}
		p.FlowM3Hour = *change.Flow
	}
	if change.TargetAt != nil {
		if change.TargetAt.IsZero() {
			return Plan{}, errors.New("target start time is required")
		}
		p.TargetAt = change.TargetAt.UTC()
	}
	if change.PreferLine != nil {
		p.PreferLine = *change.PreferLine
	}
	p.UpdatedAt = now.UTC()
	return p, nil
}

// Book is the in-memory register of production plans.
type Book struct {
	mu    sync.RWMutex
	plans map[uuid.UUID]Plan
}

func NewBook() *Book {
	return &Book{plans: make(map[uuid.UUID]Plan)}
}

// LoadBook rebuilds a book from persisted plans, giving later records
// precedence and dropping duplicates.
func LoadBook(records []Plan) (*Book, error) {
	book := NewBook()
	for _, plan := range records {
		if plan.ID == uuid.Nil {
			return nil, errors.New("persisted plan identity is incomplete")
		}
		book.plans[plan.ID] = plan
	}
	return book, nil
}

func (b *Book) Create(plan Plan) error {
	if plan.ID == uuid.Nil {
		return errors.New("plan identity is incomplete")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.plans[plan.ID]; exists {
		return errors.New("plan already exists")
	}
	b.plans[plan.ID] = plan
	return nil
}

func (b *Book) Get(id uuid.UUID) (Plan, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	plan, ok := b.plans[id]
	return plan, ok
}

// Adjust changes a waiting plan. Started or canceled plans are rejected so the
// change can never reach a live batch.
func (b *Book) Adjust(id uuid.UUID, change Adjustment, now time.Time) (Plan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	plan, ok := b.plans[id]
	if !ok {
		return Plan{}, errors.New("production plan is not found")
	}
	if plan.HasStarted() {
		return Plan{}, ErrPlanStarted
	}
	if plan.Canceled {
		return Plan{}, ErrPlanCanceled
	}
	updated, err := plan.apply(change, now)
	if err != nil {
		return Plan{}, err
	}
	b.plans[id] = updated
	return updated, nil
}

// Cancel withdraws a waiting plan.
func (b *Book) Cancel(id uuid.UUID, now time.Time) (Plan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	plan, ok := b.plans[id]
	if !ok {
		return Plan{}, errors.New("production plan is not found")
	}
	if plan.HasStarted() {
		return Plan{}, ErrPlanStarted
	}
	if plan.Canceled {
		return Plan{}, errors.New("plan is already canceled")
	}
	plan.Canceled = true
	plan.UpdatedAt = now.UTC()
	b.plans[id] = plan
	return plan, nil
}

// MarkStarted links the plan to the batch it produced.
func (b *Book) MarkStarted(id uuid.UUID, lineID model.LineID, batchID uuid.UUID, now time.Time) (Plan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	plan, ok := b.plans[id]
	if !ok {
		return Plan{}, errors.New("production plan is not found")
	}
	if plan.HasStarted() {
		return Plan{}, errors.New("plan has already started")
	}
	stamp := now.UTC()
	plan.StartedAt = &stamp
	plan.LineID = lineID
	plan.BatchID = &batchID
	plan.LastReject = nil
	plan.UpdatedAt = stamp
	b.plans[id] = plan
	return plan, nil
}

// Reject records a failed execution attempt without changing plan state.
func (b *Book) Reject(id uuid.UUID, reasons []string, now time.Time) (Plan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	plan, ok := b.plans[id]
	if !ok {
		return Plan{}, errors.New("production plan is not found")
	}
	plan.LastReject = &Rejection{At: now.UTC(), Reasons: reasons}
	plan.UpdatedAt = now.UTC()
	b.plans[id] = plan
	return plan, nil
}

// List returns all plans ordered by target time then creation time.
func (b *Book) List() []Plan {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Plan, 0, len(b.plans))
	for _, plan := range b.plans {
		result = append(result, plan)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].TargetAt.Equal(result[j].TargetAt) {
			return result[i].TargetAt.Before(result[j].TargetAt)
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}
