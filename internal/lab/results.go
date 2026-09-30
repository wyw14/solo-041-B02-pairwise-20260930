package lab

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

const (
	// ReasonLineIsolated marks a result that arrived after its line was put
	// under emergency isolation and therefore must not qualify a batch.
	ReasonLineIsolated = "line_isolated"
)

type RevisionNotice struct {
	Previous model.LabResult
	Current  model.LabResult
	Revised  bool
}

// PendingResult is a lab result that cannot be applied automatically and must
// wait for manual review (for example, a late result for an isolated line).
type PendingResult struct {
	Result     model.LabResult `json:"result"`
	Reason     string          `json:"reason"`
	ReceivedAt time.Time       `json:"received_at"`
}

type Results struct {
	mu       sync.RWMutex
	bySample map[uuid.UUID]model.LabResult
	pending  []PendingResult
}

func NewResults() *Results {
	return &Results{bySample: make(map[uuid.UUID]model.LabResult)}
}

func (r *Results) Apply(result model.LabResult) (RevisionNotice, error) {
	if !result.Valid || result.SampleID == uuid.Nil || result.Revision == 0 {
		return RevisionNotice{}, errors.New("lab result is invalid")
	}
	r.mu.Lock()
	previous, exists := r.bySample[result.SampleID]
	if exists && !result.NewerThan(previous) {
		r.mu.Unlock()
		return RevisionNotice{}, errors.New("lab result revision is stale")
	}
	r.bySample[result.SampleID] = result
	r.mu.Unlock()
	notice := RevisionNotice{Previous: previous, Current: result, Revised: exists}
	return notice, nil
}

// HoldForReview files a result into the review queue without applying it to any
// batch. Repeated submissions of the same sample revision are ignored so a late
// result cannot flood the queue.
func (r *Results) HoldForReview(result model.LabResult, reason string, now time.Time) PendingResult {
	entry := PendingResult{Result: result, Reason: reason, ReceivedAt: now.UTC()}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pending := range r.pending {
		if pending.Result.SampleID == result.SampleID && pending.Result.Revision == result.Revision {
			return pending
		}
	}
	r.pending = append(r.pending, entry)
	return entry
}

func (r *Results) Get(sampleID uuid.UUID) (model.LabResult, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result, ok := r.bySample[sampleID]
	return result, ok
}

// Pending returns every result awaiting manual review, oldest first.
func (r *Results) Pending() []PendingResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]PendingResult(nil), r.pending...)
}

// PendingForLine returns the review queue for a single line, oldest first.
func (r *Results) PendingForLine(lineID model.LineID) []PendingResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]PendingResult, 0)
	for _, pending := range r.pending {
		if pending.Result.LineID == lineID {
			result = append(result, pending)
		}
	}
	return result
}
