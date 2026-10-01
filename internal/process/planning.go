package process

import (
	"github.com/wyw14/cry-97/internal/intake"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/plan"
)

// LineSnapshot returns the current state of a process line.
func (p *Plant) LineSnapshot(id model.LineID) (model.LineState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state, ok := p.lines[id]
	return state, ok
}

// IntakeCapacity returns the configured intake limit of a process line.
func (p *Plant) IntakeCapacity(lineID model.LineID) (float64, bool) {
	return p.intake.Capacity(lineID)
}

// EqualizerSnapshot returns the current equalization basin occupancy.
func (p *Plant) EqualizerSnapshot() []intake.Equalizer {
	return p.equalizers.Snapshot()
}

// PlanBoard exposes the cross-line production plan.
func (p *Plant) PlanBoard() *plan.Board {
	return p.planBoard
}
