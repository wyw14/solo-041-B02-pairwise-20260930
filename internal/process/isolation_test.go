package process

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/lab"
	"github.com/wyw14/cry-97/internal/model"
)

func newTestPlant(t *testing.T) *Plant {
	t.Helper()
	line := model.LineID("line-1")
	clock := func() time.Time { return time.Now().UTC() }
	plant, err := NewPlant(t.TempDir(), []model.LineID{line}, clock)
	if err != nil {
		t.Fatalf("NewPlant: %v", err)
	}
	return plant
}

func qualifiedResult(line model.LineID, batchID uuid.UUID) model.LabResult {
	return model.LabResult{
		SampleID: uuid.New(), LineID: line, BatchID: batchID, Revision: 1,
		COD: 30, Ammonia: 2, Valid: true, ReceivedAt: time.Now().UTC(),
	}
}

// TestIsolatedLineRejectsLateLabResult is the incident regression: once a line
// is emergency isolated and its equipment stopped, a late qualified lab result
// for the original batch must not issue a permit or open the valve. It must be
// filed for review and leave both the batch and the discharge controls untouched.
func TestIsolatedLineRejectsLateLabResult(t *testing.T) {
	ctx := context.Background()
	plant := newTestPlant(t)
	line := model.LineID("line-1")

	batch, err := plant.StartBatch(ctx, line, 500, "influent")
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	for _, stage := range []model.ProcessStage{
		model.StageEqualizing, model.StageBiological, model.StageSettling,
	} {
		if batch, err = plant.AdvanceBatch(ctx, line, stage); err != nil {
			t.Fatalf("AdvanceBatch %s: %v", stage, err)
		}
	}

	// A normal qualified result qualifies the batch, issues a permit and opens the valve.
	result := qualifiedResult(line, batch.ID)
	outcome, err := plant.ApplyLabResult(ctx, result, plant.Clock())
	if err != nil {
		t.Fatalf("ApplyLabResult qualified: %v", err)
	}
	if outcome.Permit == nil || !outcome.Permit.Active {
		t.Fatalf("expected an active permit, got %+v", outcome.Permit)
	}
	if !outcome.Batch.Qualified || outcome.Batch.Stage != model.StageQualified {
		t.Fatalf("expected qualified batch, got %+v", outcome.Batch)
	}
	if valve := valveFor(plant, line); !valve.Open || valve.PermitID == uuid.Nil {
		t.Fatalf("expected open valve on permit, got %+v", valve)
	}

	// Emergency isolation stops the line and must invalidate the discharge chain.
	if _, err := plant.EmergencyStop(ctx, line, "contamination alarm"); err != nil {
		t.Fatalf("EmergencyStop: %v", err)
	}
	state := plant.Lines()[0]
	if !state.Emergency || state.Stage != model.StageIsolated {
		t.Fatalf("expected isolated line, got %+v", state)
	}
	permitAfterIsolation, _ := plant.permits.Current(batch.ID)
	if permitAfterIsolation.Active || permitAfterIsolation.RevokedAt == nil {
		t.Fatalf("expected permit revoked on isolation, got %+v", permitAfterIsolation)
	}
	if valve := valveFor(plant, line); valve.Open || valve.PermitID != uuid.Nil {
		t.Fatalf("expected valve held and released on isolation, got %+v", valve)
	}
	if !plant.valves.Isolated(line) {
		t.Fatal("expected valve controller to mark the line isolated")
	}

	// The late qualified result from the original batch arrives after isolation.
	batchBeforeLate, _ := plant.ActiveBatch(line)
	late := qualifiedResult(line, batch.ID)
	lateOutcome, err := plant.ApplyLabResult(ctx, late, plant.Clock())
	if err != nil {
		t.Fatalf("late result should be accepted for review, got error %v", err)
	}
	if lateOutcome.Pending == nil || lateOutcome.Pending.Reason != lab.ReasonLineIsolated {
		t.Fatalf("expected pending review entry, got %+v", lateOutcome.Pending)
	}
	if lateOutcome.Permit != nil {
		t.Fatalf("late result must not issue a permit, got %+v", lateOutcome.Permit)
	}

	// Batch discharge state must be byte-for-byte unchanged.
	batchAfterLate, _ := plant.ActiveBatch(line)
	if batchAfterLate != batchBeforeLate {
		t.Fatalf("late result changed the batch:\nbefore=%+v\nafter =%+v", batchBeforeLate, batchAfterLate)
	}
	// The late result must not be applied as the authoritative lab result.
	if _, applied := plant.labResults.Get(late.SampleID); applied {
		t.Fatal("late result must not enter the applied result store")
	}
	if pending := plant.PendingLabResults(line); len(pending) != 1 || pending[0].Result.SampleID != late.SampleID {
		t.Fatalf("expected exactly one pending review entry, got %+v", pending)
	}

	// No new permit and the old one stays revoked; valve stays shut and locked.
	permitAfterLate, _ := plant.permits.Current(batch.ID)
	if permitAfterLate.Active {
		t.Fatalf("old permit must not remain active, got %+v", permitAfterLate)
	}
	if valve := valveFor(plant, line); valve.Open || valve.PermitID != uuid.Nil {
		t.Fatalf("valve must stay closed after late result, got %+v", valve)
	}
}

func valveFor(plant *Plant, line model.LineID) dischargeValve {
	for _, valve := range plant.valves.Snapshot() {
		if valve.LineID == line {
			return dischargeValve{Open: valve.Open, PermitID: valve.PermitID}
		}
	}
	return dischargeValve{}
}

type dischargeValve struct {
	Open     bool
	PermitID uuid.UUID
}

// TestUnqualifiedResultHoldsValveWithoutIsolation keeps the non-emergency
// path honest: a disqualifying revision revokes the permit and holds the valve.
func TestUnqualifiedResultHoldsValveWithoutIsolation(t *testing.T) {
	ctx := context.Background()
	plant := newTestPlant(t)
	line := model.LineID("line-1")

	batch, err := plant.StartBatch(ctx, line, 500, "influent")
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	if batch, err = plant.AdvanceBatch(ctx, line, model.StageEqualizing); err != nil {
		t.Fatalf("AdvanceBatch: %v", err)
	}
	if batch, err = plant.AdvanceBatch(ctx, line, model.StageBiological); err != nil {
		t.Fatalf("AdvanceBatch: %v", err)
	}
	if batch, err = plant.AdvanceBatch(ctx, line, model.StageSettling); err != nil {
		t.Fatalf("AdvanceBatch: %v", err)
	}

	result := qualifiedResult(line, batch.ID)
	if _, err := plant.ApplyLabResult(ctx, result, plant.Clock()); err != nil {
		t.Fatalf("ApplyLabResult qualified: %v", err)
	}

	bad := result
	bad.Revision = 2
	bad.COD = 200 // over the 50 limit
	outcome, err := plant.ApplyLabResult(ctx, bad, plant.Clock())
	if err != nil {
		t.Fatalf("ApplyLabResult unqualified: %v", err)
	}
	if outcome.Permit != nil && outcome.Permit.Active {
		t.Fatalf("expected revoked permit, got %+v", outcome.Permit)
	}
	if outcome.Batch.Qualified {
		t.Fatal("expected batch to lose qualified status")
	}
	if valve := valveFor(plant, line); valve.Open || valve.PermitID != uuid.Nil {
		t.Fatalf("expected valve held after disqualifying result, got %+v", valve)
	}
}
