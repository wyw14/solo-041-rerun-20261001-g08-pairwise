package plan

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Status describes where a planned batch sits in its lifecycle.
type Status string

const (
	StatusWaiting   Status = "waiting"   // 等待启动
	StatusRunning   Status = "running"   // 已启动，批次运行中
	StatusCompleted Status = "completed" // 启动的批次已结束
	StatusBlocked   Status = "blocked"   // 现场校验未通过，无法启动
	StatusCancelled Status = "cancelled" // 启动前被取消
)

// PlannedBatch is a batch the operator intends to start on a process line.
type PlannedBatch struct {
	ID          uuid.UUID    `json:"id"`
	LineID      model.LineID `json:"line_id"`
	Source      string       `json:"source"`
	FlowM3Hour  float64      `json:"flow_m3_hour"`
	TargetStart time.Time    `json:"target_start"`
	Status      Status       `json:"status"`
	Reason      string       `json:"reason,omitempty"`
	BatchID     uuid.UUID    `json:"batch_id,omitempty"`
	Revision    uint64       `json:"revision"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	StartedAt   time.Time    `json:"started_at,omitempty"`
	CompletedAt time.Time    `json:"completed_at,omitempty"`
}

func NewPlannedBatch(lineID model.LineID, source string, flow float64, targetStart time.Time, now time.Time) (PlannedBatch, error) {
	if lineID == "" {
		return PlannedBatch{}, errors.New("process line is required")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return PlannedBatch{}, errors.New("intake source is required")
	}
	if flow <= 0 {
		return PlannedBatch{}, errors.New("planned flow must be positive")
	}
	if targetStart.IsZero() {
		return PlannedBatch{}, errors.New("target start time is required")
	}
	stamp := now.UTC()
	return PlannedBatch{
		ID: uuid.New(), LineID: lineID, Source: source, FlowM3Hour: flow,
		TargetStart: targetStart.UTC(), Status: StatusWaiting, Revision: 1,
		CreatedAt: stamp, UpdatedAt: stamp,
	}, nil
}

// adjustable reports whether the plan may still be revised or cancelled.
// Plans that already started (running/completed) are immutable by design.
func (p PlannedBatch) adjustable() error {
	switch p.Status {
	case StatusWaiting, StatusBlocked:
		return nil
	case StatusRunning:
		return errors.New("planned batch has already started and cannot be changed")
	case StatusCompleted:
		return errors.New("planned batch is already completed and cannot be changed")
	default:
		return errors.New("planned batch is cancelled and cannot be changed")
	}
}
