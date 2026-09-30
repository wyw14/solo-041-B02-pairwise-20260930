package lab

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// ReviewReasonIsolated marks a result that arrived while the line was under
// emergency isolation and therefore could not qualify its batch.
const ReviewReasonIsolated = "process line is isolated"

type RevisionNotice struct {
	Previous model.LabResult
	Current  model.LabResult
	Revised  bool
}

// ReviewRecord holds a lab result that could not be applied to its batch
// (currently: the line is isolated). The result is quarantined here for
// manual review and never touches the batch, permits or valves on its own.
type ReviewRecord struct {
	Result   model.LabResult `json:"result"`
	Reason   string          `json:"reason"`
	StoredAt time.Time       `json:"stored_at"`
}

type Results struct {
	mu       sync.RWMutex
	bySample map[uuid.UUID]model.LabResult
	review   map[uuid.UUID]ReviewRecord
}

func NewResults() *Results {
	return &Results{
		bySample: make(map[uuid.UUID]model.LabResult),
		review:   make(map[uuid.UUID]ReviewRecord),
	}
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

// StageForReview quarantines a result instead of applying it. Only the newest
// revision per sample is retained.
func (r *Results) StageForReview(result model.LabResult, reason string, now time.Time) ReviewRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reason == "" {
		reason = "result deferred for manual review"
	}
	if prior, ok := r.review[result.SampleID]; ok && result.Revision <= prior.Result.Revision {
		return prior
	}
	record := ReviewRecord{Result: result, Reason: reason, StoredAt: now.UTC()}
	r.review[result.SampleID] = record
	return record
}

func (r *Results) AcknowledgeReview(sampleID uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.review[sampleID]; !ok {
		return false
	}
	delete(r.review, sampleID)
	return true
}

func (r *Results) ReviewRecords() []ReviewRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	records := make([]ReviewRecord, 0, len(r.review))
	for _, record := range r.review {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].StoredAt.Before(records[j].StoredAt)
	})
	return records
}

func (r *Results) Get(sampleID uuid.UUID) (model.LabResult, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result, ok := r.bySample[sampleID]
	return result, ok
}
