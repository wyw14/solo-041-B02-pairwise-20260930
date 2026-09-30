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
	// Pending is set when the result could not be applied to the batch and was
	// filed for manual review instead (for example, a late result for a line
	// under emergency isolation). In that case Batch is left untouched.
	Pending *lab.PendingResult `json:"pending_review,omitempty"`
}

func (p *Plant) SubmitLabPayload(ctx context.Context, payload []byte) (QualificationOutcome, error) {
	now := p.now()
	result, decodeErr := lab.Decode(payload, now)
	outcome, applyErr := p.ApplyLabResult(ctx, result, now)
	if decodeErr != nil {
		return QualificationOutcome{}, decodeErr
	}
	if applyErr != nil {
		return QualificationOutcome{}, applyErr
	}
	return outcome, nil
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
	if batch.LineID != result.LineID {
		p.mu.Unlock()
		return QualificationOutcome{}, errors.New("lab result does not belong to the batch line")
	}
	state, lineOK := p.lines[result.LineID]
	if !lineOK {
		p.mu.Unlock()
		return QualificationOutcome{}, errors.New("lab result line is not found")
	}
	// A line under emergency isolation is out of service. A late (or revised)
	// lab result must not qualify the batch, issue a permit, or move the valve;
	// it is filed for manual review only and the batch keeps its current state.
	if state.Emergency || state.Stage == model.StageIsolated {
		p.mu.Unlock()
		pending := p.labResults.HoldForReview(result, lab.ReasonLineIsolated, now)
		return QualificationOutcome{Batch: batch, Pending: &pending}, nil
	}
	updated, err := batch.ApplyQualification(result, now)
	if err != nil {
		p.mu.Unlock()
		return QualificationOutcome{}, err
	}
	p.batches[result.BatchID] = updated
	line := p.lines[updated.LineID]
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

// PendingLabResults returns every lab result awaiting manual review, oldest
// first. When lineID is non-empty it is filtered to that line.
func (p *Plant) PendingLabResults(lineID model.LineID) []lab.PendingResult {
	if lineID == "" {
		return p.labResults.Pending()
	}
	return p.labResults.PendingForLine(lineID)
}
