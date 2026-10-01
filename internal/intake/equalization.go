package intake

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

type Equalizer struct {
	ID           string       `json:"id"`
	Capacity     float64      `json:"capacity"`
	AssignedFlow float64      `json:"assigned_flow"`
	LineID       model.LineID `json:"line_id,omitempty"`
	AssignedAt   time.Time    `json:"assigned_at,omitempty"`
}

type EqualizationPool struct {
	mu     sync.Mutex
	basins map[string]Equalizer
}

func NewEqualizationPool(basins []Equalizer) (*EqualizationPool, error) {
	if len(basins) == 0 {
		return nil, errors.New("at least one equalizer is required")
	}
	pool := &EqualizationPool{basins: make(map[string]Equalizer, len(basins))}
	for _, basin := range basins {
		if basin.ID == "" || basin.Capacity <= 0 {
			return nil, errors.New("equalizer identity and capacity are required")
		}
		pool.basins[basin.ID] = basin
	}
	return pool, nil
}

func (p *EqualizationPool) Assign(lineID model.LineID, flow float64, now time.Time) (Equalizer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.basins))
	for id := range p.basins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		basin := p.basins[id]
		if basin.LineID == lineID && flow <= basin.Capacity {
			basin.AssignedFlow = flow
			basin.AssignedAt = now.UTC()
			p.basins[id] = basin
			return basin, nil
		}
	}
	for _, id := range ids {
		basin := p.basins[id]
		if basin.LineID == "" && flow <= basin.Capacity {
			basin.LineID = lineID
			basin.AssignedFlow = flow
			basin.AssignedAt = now.UTC()
			p.basins[id] = basin
			return basin, nil
		}
	}
	return Equalizer{}, errors.New("no equalizer can accept the requested flow")
}

func (p *EqualizationPool) Release(lineID model.LineID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, basin := range p.basins {
		if basin.LineID != lineID {
			continue
		}
		basin.LineID = ""
		basin.AssignedFlow = 0
		basin.AssignedAt = time.Time{}
		p.basins[id] = basin
	}
}

func (p *EqualizationPool) Snapshot() []Equalizer {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]Equalizer, 0, len(p.basins))
	for _, basin := range p.basins {
		result = append(result, basin)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
