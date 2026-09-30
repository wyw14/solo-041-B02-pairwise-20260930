package process

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/lab"
	"github.com/wyw14/cry-97/internal/model"
)

type QualificationOutcome struct {
	Batch  model.Batch       `json:"batch"`
	Permit *discharge.Permit `json:"permit,omitempty"`
	Review *lab.ReviewRecord `json:"review,omitempty"`
}

func (p *Plant) SubmitLabPayload(ctx context.Context, payload []byte) (QualificationOutcome, error) {
	now := p.now()
	result, err := lab.Decode(payload, now)
	if err != nil {
		return QualificationOutcome{}, err
	}
	return p.ApplyLabResult(ctx, result, now)
}

func (p *Plant) ApplyLabResult(ctx context.Context, result model.LabResult, now time.Time) (QualificationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return QualificationOutcome{}, err
	}
	if !result.Valid || result.BatchID == uuid.Nil {
		return QualificationOutcome{}, errors.New("invalid lab result cannot qualify a batch")
	}
	p.mu.Lock()
	batch, exists := p.batches[result.BatchID]
	if !exists {
		p.mu.Unlock()
		return QualificationOutcome{}, errors.New("lab result batch is not found")
	}
	if result.LineID != batch.LineID {
		p.mu.Unlock()
		return QualificationOutcome{}, errors.New("lab result line does not match batch")
	}
	line := p.lines[batch.LineID]
	if line.Emergency || line.Stage == model.StageIsolated {
		// Late results for an isolated line never qualify the batch, issue
		// permits or move valves: they are quarantined for manual review and
		// the batch keeps its pre-isolation discharge state.
		p.mu.Unlock()
		record := p.labResults.StageForReview(result, lab.ReviewReasonIsolated, now)
		return QualificationOutcome{Batch: batch, Review: &record}, nil
	}
	updated, err := batch.ApplyQualification(result, now)
	if err != nil {
		p.mu.Unlock()
		return QualificationOutcome{}, err
	}
	p.batches[result.BatchID] = updated
	line.Stage = updated.Stage
	line.UpdatedAt = now.UTC()
	p.lines[updated.LineID] = line
	p.mu.Unlock()
	if _, err := p.labResults.Apply(result); err != nil {
		return QualificationOutcome{}, err
	}
	permit, permitErr := p.permits.ApplyResult(result, now)
	outcome := QualificationOutcome{Batch: updated}
	if permitErr == nil {
		outcome.Permit = &permit
	} else if result.Qualified() {
		return QualificationOutcome{}, permitErr
	}
	return outcome, nil
}

func (p *Plant) Permits() []discharge.Permit {
	return p.permits.All()
}

// ReviewRecords lists lab results deferred for manual review (e.g. results
// that arrived while their line was under emergency isolation).
func (p *Plant) ReviewRecords() []lab.ReviewRecord {
	return p.labResults.ReviewRecords()
}
