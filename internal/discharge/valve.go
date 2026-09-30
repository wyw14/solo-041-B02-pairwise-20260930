package discharge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type Valve struct {
	LineID    model.LineID     `json:"line_id"`
	Kind      model.DeviceKind `json:"kind"`
	Open      bool             `json:"open"`
	PermitID  uuid.UUID        `json:"permit_id,omitempty"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type ValveController struct {
	mu       sync.RWMutex
	valves   map[model.LineID]Valve
	isolated map[model.LineID]bool
}

func NewValveController(lines []model.LineID) (*ValveController, error) {
	if len(lines) == 0 {
		return nil, errors.New("discharge lines are required")
	}
	controller := &ValveController{
		valves:   make(map[model.LineID]Valve, len(lines)),
		isolated: make(map[model.LineID]bool, len(lines)),
	}
	for _, lineID := range lines {
		if lineID == "" {
			return nil, errors.New("discharge line is empty")
		}
		controller.valves[lineID] = Valve{LineID: lineID, Kind: model.DeviceValve}
	}
	return controller, nil
}

// ErrLineIsolated reports that a discharge action was refused because the
// process line is under emergency isolation.
var ErrLineIsolated = errors.New("discharge line is isolated")

func (c *ValveController) Open(lineID model.LineID, permitID uuid.UUID, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok || permitID == uuid.Nil {
		return errors.New("discharge valve target is invalid")
	}
	if c.isolated[lineID] {
		return ErrLineIsolated
	}
	valve.Open = true
	valve.PermitID = permitID
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return nil
}

func (c *ValveController) Hold(lineID model.LineID, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok {
		return errors.New("discharge valve is not found")
	}
	valve.Open = false
	valve.PermitID = uuid.Nil
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return nil
}

// Isolate forcibly closes the line's discharge valve and refuses every later
// Open until Release is called. Holding the valve is idempotent.
func (c *ValveController) Isolate(lineID model.LineID, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	valve, ok := c.valves[lineID]
	if !ok {
		return errors.New("discharge valve is not found")
	}
	c.isolated[lineID] = true
	valve.Open = false
	valve.PermitID = uuid.Nil
	valve.UpdatedAt = now.UTC()
	c.valves[lineID] = valve
	return nil
}

// Release clears the isolation lock after the emergency has been resolved; it
// does not reopen the valve, a new permit is required for that.
func (c *ValveController) Release(lineID model.LineID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.isolated, lineID)
}

func (c *ValveController) Isolated(lineID model.LineID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isolated[lineID]
}

func (c *ValveController) Snapshot() []Valve {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Valve, 0, len(c.valves))
	for _, valve := range c.valves {
		result = append(result, valve)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LineID < result[j].LineID })
	return result
}
