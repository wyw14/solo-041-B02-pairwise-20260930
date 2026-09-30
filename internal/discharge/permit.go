package discharge

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

type Permit struct {
	ID             uuid.UUID    `json:"id"`
	LineID         model.LineID `json:"line_id"`
	BatchID        uuid.UUID    `json:"batch_id"`
	SampleID       uuid.UUID    `json:"sample_id"`
	SampleRevision uint64       `json:"sample_revision"`
	Active         bool         `json:"active"`
	IssuedAt       time.Time    `json:"issued_at"`
	RevokedAt      *time.Time   `json:"revoked_at,omitempty"`
}

type PermitBook struct {
	mu       sync.RWMutex
	byBatch  map[uuid.UUID]Permit
	valves   *ValveController
	isolated map[model.LineID]bool
}

func NewPermitBook(valves *ValveController) (*PermitBook, error) {
	if valves == nil {
		return nil, errors.New("permit book requires valve controller")
	}
	return &PermitBook{
		byBatch:  make(map[uuid.UUID]Permit),
		valves:   valves,
		isolated: make(map[model.LineID]bool),
	}, nil
}

func (b *PermitBook) ApplyResult(result model.LabResult, now time.Time) (Permit, error) {
	if !result.Valid || result.SampleID == uuid.Nil || result.BatchID == uuid.Nil || result.Revision == 0 {
		return Permit{}, errors.New("permit result is invalid")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.isolated[result.LineID] {
		// An isolated line can neither receive new permits nor have its permit
		// book touched by late lab results; review happens out of band.
		return Permit{}, ErrLineIsolated
	}
	current, exists := b.byBatch[result.BatchID]
	if exists && current.SampleID == result.SampleID && result.Revision <= current.SampleRevision {
		return current, errors.New("permit result revision is stale")
	}
	if !result.Qualified() {
		if exists && current.Active {
			stamp := now.UTC()
			current.Active = false
			current.SampleRevision = result.Revision
			current.RevokedAt = &stamp
			b.byBatch[result.BatchID] = current
			if err := b.valves.Hold(result.LineID, now); err != nil {
				return Permit{}, err
			}
			return current, nil
		}
		return Permit{}, errors.New("result is not qualified for discharge")
	}
	permit := Permit{
		ID: uuid.New(), LineID: result.LineID, BatchID: result.BatchID,
		SampleID: result.SampleID, SampleRevision: result.Revision, Active: true, IssuedAt: now.UTC(),
	}
	if err := b.valves.Open(result.LineID, permit.ID, now); err != nil {
		return Permit{}, err
	}
	b.byBatch[result.BatchID] = permit
	return permit, nil
}

// Isolate revokes every active permit on the line and locks the discharge
// valve. Late results afterwards are refused until Release is called.
func (b *PermitBook) Isolate(lineID model.LineID, now time.Time) error {
	b.mu.Lock()
	b.isolated[lineID] = true
	stamp := now.UTC()
	for batchID, permit := range b.byBatch {
		if permit.LineID != lineID || !permit.Active {
			continue
		}
		permit.Active = false
		permit.RevokedAt = &stamp
		b.byBatch[batchID] = permit
	}
	b.mu.Unlock()
	return b.valves.Isolate(lineID, now)
}

// Release clears the isolation lock; revoked permits stay revoked and a fresh
// qualified result is required to discharge again.
func (b *PermitBook) Release(lineID model.LineID) {
	b.mu.Lock()
	delete(b.isolated, lineID)
	b.mu.Unlock()
	b.valves.Release(lineID)
}

func (b *PermitBook) Isolated(lineID model.LineID) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.isolated[lineID]
}

func (b *PermitBook) Current(batchID uuid.UUID) (Permit, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	permit, ok := b.byBatch[batchID]
	return permit, ok
}

func (b *PermitBook) All() []Permit {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]Permit, 0, len(b.byBatch))
	for _, permit := range b.byBatch {
		result = append(result, permit)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IssuedAt.After(result[j].IssuedAt) })
	return result
}
