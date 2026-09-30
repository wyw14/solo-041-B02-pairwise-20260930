package process

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/discharge"
	"github.com/wyw14/cry-97/internal/model"
)

func newTestPlant(t *testing.T) (*Plant, model.LineID, string) {
	t.Helper()
	lineID := model.LineID("L1")
	clock := func() time.Time { return time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) }
	dataDir := t.TempDir()
	plant, err := NewPlant(dataDir, []model.LineID{lineID}, clock)
	if err != nil {
		t.Fatalf("NewPlant: %v", err)
	}
	return plant, lineID, dataDir
}

func qualifiedPayload(lineID model.LineID, batchID uuid.UUID, revision uint64) []byte {
	return []byte(`{"sample_id":"` + uuid.NewString() + `","line_id":"` + string(lineID) +
		`","batch_id":"` + batchID.String() + `","revision":` +
		uintToString(revision) + `,"cod":30,"ammonia":2}`)
}

func unqualifiedPayload(lineID model.LineID, batchID uuid.UUID, revision uint64) []byte {
	return []byte(`{"sample_id":"` + uuid.NewString() + `","line_id":"` + string(lineID) +
		`","batch_id":"` + batchID.String() + `","revision":` +
		uintToString(revision) + `,"cod":90,"ammonia":8}`)
}

func uintToString(v uint64) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

func valveFor(plant *Plant, lineID model.LineID) discharge.Valve {
	for _, valve := range plant.valves.Snapshot() {
		if valve.LineID == lineID {
			return valve
		}
	}
	return discharge.Valve{}
}

// TestLateQualifiedResultAfterIsolation reproduces the incident: the line is
// emergency-isolated and its devices stopped, then the original batch's
// qualified lab result arrives late. It must not issue a permit, open the
// valve or touch the batch; it goes to review instead, and the old permit is
// revoked by the isolation itself.
func TestLateQualifiedResultAfterIsolation(t *testing.T) {
	ctx := context.Background()
	plant, lineID, _ := newTestPlant(t)

	batch, err := plant.StartBatch(ctx, lineID, 100, "influent")
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	for _, stage := range []model.ProcessStage{
		model.StageEqualizing, model.StageBiological, model.StageSettling,
	} {
		if _, err := plant.AdvanceBatch(ctx, lineID, stage); err != nil {
			t.Fatalf("AdvanceBatch %s: %v", stage, err)
		}
	}

	outcome, err := plant.SubmitLabPayload(ctx, qualifiedPayload(lineID, batch.ID, 1))
	if err != nil {
		t.Fatalf("qualified result before isolation: %v", err)
	}
	if outcome.Permit == nil || !outcome.Permit.Active {
		t.Fatal("expected active permit before isolation")
	}
	originalPermitID := outcome.Permit.ID
	if !valveFor(plant, lineID).Open {
		t.Fatal("expected valve open before isolation")
	}
	qualifiedBatch := outcome.Batch
	if !qualifiedBatch.Qualified || qualifiedBatch.Stage != model.StageQualified {
		t.Fatalf("batch not qualified: %+v", qualifiedBatch)
	}

	if _, err := plant.EmergencyStop(ctx, lineID, "upstream spill"); err != nil {
		t.Fatalf("EmergencyStop: %v", err)
	}

	// Isolation must revoke the existing permit and slam the valve shut.
	revoked, ok := plant.permits.Current(batch.ID)
	if !ok {
		t.Fatal("permit record disappeared after isolation")
	}
	if revoked.Active || revoked.RevokedAt == nil {
		t.Fatalf("old permit still effective: %+v", revoked)
	}
	if valve := valveFor(plant, lineID); valve.Open || valve.PermitID != uuid.Nil {
		t.Fatalf("valve not secured after isolation: %+v", valve)
	}
	if !plant.valves.Isolated(lineID) {
		t.Fatal("valve controller not marked isolated")
	}

	// Late qualified result from the original batch.
	late, err := plant.SubmitLabPayload(ctx, qualifiedPayload(lineID, batch.ID, 2))
	if err != nil {
		t.Fatalf("late result should be quarantined, not rejected: %v", err)
	}
	if late.Permit != nil {
		t.Fatalf("late result issued a permit: %+v", late.Permit)
	}
	if late.Review == nil || late.Review.Result.Revision != 2 {
		t.Fatalf("late result not staged for review: %+v", late.Review)
	}
	records := plant.ReviewRecords()
	if len(records) != 1 || records[0].Result.Revision != 2 {
		t.Fatalf("expected one review record, got %+v", records)
	}

	// Batch discharge state must be untouched.
	after, ok := plant.ActiveBatch(lineID)
	if !ok {
		t.Fatal("batch lost")
	}
	if after.Qualified != qualifiedBatch.Qualified ||
		after.Stage != qualifiedBatch.Stage ||
		after.QualificationRev != qualifiedBatch.QualificationRev ||
		after.QualificationSample != qualifiedBatch.QualificationSample {
		t.Fatalf("batch changed after late result:\nbefore=%+v\nafter =%+v", qualifiedBatch, after)
	}

	// No new permit may appear and the old one must stay revoked; valve shut.
	if current := plant.Permits(); len(current) != 1 || current[0].ID != originalPermitID || current[0].Active {
		t.Fatalf("permit book altered by late result: %+v", current)
	}
	if valve := valveFor(plant, lineID); valve.Open {
		t.Fatalf("valve reopened by late result: %+v", valve)
	}

	// A late UNqualified result must also be unable to flip the batch's
	// discharge state or revalidate/revoke anything.
	if _, err := plant.SubmitLabPayload(ctx, unqualifiedPayload(lineID, batch.ID, 3)); err != nil {
		t.Fatalf("late unqualified result should be quarantined: %v", err)
	}
	after2, _ := plant.ActiveBatch(lineID)
	if !after2.Qualified {
		t.Fatal("late unqualified result flipped batch to not qualified")
	}
	if current, _ := plant.permits.Current(batch.ID); current.Active {
		t.Fatal("permit reactivated by late result")
	}
	if valve := valveFor(plant, lineID); valve.Open {
		t.Fatal("valve opened while isolated")
	}
	if len(plant.ReviewRecords()) != 2 {
		t.Fatalf("expected two review records, got %d", len(plant.ReviewRecords()))
	}
}

// TestIsolationSurvivesRecovery ensures that after a restart the replayed
// emergency event re-secures permits and valves, and late results stay
// quarantined.
func TestIsolationSurvivesRecovery(t *testing.T) {
	ctx := context.Background()
	plant, lineID, dataDir := newTestPlant(t)

	batch, err := plant.StartBatch(ctx, lineID, 100, "influent")
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	if _, err := plant.SubmitLabPayload(ctx, qualifiedPayload(lineID, batch.ID, 1)); err != nil {
		t.Fatalf("qualified result: %v", err)
	}
	if _, err := plant.EmergencyStop(ctx, lineID, "upstream spill"); err != nil {
		t.Fatalf("EmergencyStop: %v", err)
	}

	restarted, err := NewPlant(dataDir, []model.LineID{lineID}, plant.now)
	if err != nil {
		t.Fatalf("restart NewPlant: %v", err)
	}
	if _, err := restarted.RecoverLine(ctx, lineID); err != nil {
		t.Fatalf("RecoverLine: %v", err)
	}

	line := restarted.Lines()[0]
	if !line.Emergency || line.Stage != model.StageIsolated {
		t.Fatalf("line not isolated after recovery: %+v", line)
	}
	if valve := valveFor(restarted, lineID); valve.Open || !restarted.valves.Isolated(lineID) {
		t.Fatalf("valve not secured after recovery: %+v", valve)
	}

	// The permit/valve interlock must stay armed after replay, so any late
	// qualified result is refused rather than issuing a new permit.
	late := model.LabResult{
		SampleID: uuid.New(), LineID: lineID, BatchID: batch.ID,
		Revision: 2, COD: 30, Ammonia: 2, Valid: true,
		ReceivedAt: restarted.now(),
	}
	if _, err := restarted.permits.ApplyResult(late, restarted.now()); err != discharge.ErrLineIsolated {
		t.Fatalf("expected permit refusal ErrLineIsolated after recovery, got %v", err)
	}
	if valve := valveFor(restarted, lineID); valve.Open {
		t.Fatal("valve opened after recovery despite isolation")
	}
}

// TestValveRejectsOpenWhileIsolated guards the valve-layer check directly.
func TestValveRejectsOpenWhileIsolated(t *testing.T) {
	lineID := model.LineID("L1")
	valves, err := discharge.NewValveController([]model.LineID{lineID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	permitID := uuid.New()
	if err := valves.Open(lineID, permitID, now); err != nil {
		t.Fatalf("open before isolation: %v", err)
	}
	if err := valves.Isolate(lineID, now); err != nil {
		t.Fatalf("isolate: %v", err)
	}
	if err := valves.Open(lineID, permitID, now); err != discharge.ErrLineIsolated {
		t.Fatalf("expected ErrLineIsolated, got %v", err)
	}
	valves.Release(lineID)
	if err := valves.Open(lineID, permitID, now); err != nil {
		t.Fatalf("open after release: %v", err)
	}
}
