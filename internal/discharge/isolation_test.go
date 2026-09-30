package discharge

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

var testLine = model.LineID("line-1")

func newRig(t *testing.T) (*ValveController, *PermitBook) {
	t.Helper()
	valves, err := NewValveController([]model.LineID{testLine})
	if err != nil {
		t.Fatalf("NewValveController: %v", err)
	}
	book, err := NewPermitBook(valves)
	if err != nil {
		t.Fatalf("NewPermitBook: %v", err)
	}
	return valves, book
}

func qualifiedResult() model.LabResult {
	return model.LabResult{
		SampleID: uuid.New(), LineID: testLine, BatchID: uuid.New(), Revision: 1,
		COD: 30, Ammonia: 2, Valid: true,
	}
}

func valveOpen(valves *ValveController) bool {
	for _, valve := range valves.Snapshot() {
		if valve.LineID == testLine {
			return valve.Open
		}
	}
	return false
}

func TestPermitRefusedOnIsolatedLine(t *testing.T) {
	now := time.Now().UTC()
	valves, book := newRig(t)

	if err := valves.Isolate(testLine, now); err != nil {
		t.Fatalf("Isolate: %v", err)
	}
	if _, err := book.ApplyResult(qualifiedResult(), now); !errors.Is(err, ErrLineIsolated) {
		t.Fatalf("expected ErrLineIsolated, got %v", err)
	}
	// A direct valve open must also be refused.
	if err := valves.Open(testLine, uuid.New(), now); !errors.Is(err, ErrLineIsolated) {
		t.Fatalf("expected ErrLineIsolated from Open, got %v", err)
	}
	if valveOpen(valves) {
		t.Fatal("valve must stay closed on an isolated line")
	}
}

func TestRevokeLineClosesPermitAndValve(t *testing.T) {
	now := time.Now().UTC()
	valves, book := newRig(t)
	result := qualifiedResult()

	permit, err := book.ApplyResult(result, now)
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	if !permit.Active || !valveOpen(valves) {
		t.Fatal("expected active permit and open valve before isolation")
	}

	revoked, err := book.RevokeLine(testLine, now)
	if err != nil {
		t.Fatalf("RevokeLine: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("expected one revoked permit, got %d", revoked)
	}
	current, _ := book.Current(result.BatchID)
	if current.Active || current.RevokedAt == nil {
		t.Fatalf("expected revoked permit, got %+v", current)
	}
	if valveOpen(valves) {
		t.Fatal("expected valve closed after RevokeLine")
	}
	if !valves.Isolated(testLine) {
		t.Fatal("expected line marked isolated after RevokeLine")
	}

	// Re-submitting an even newer qualified result after isolation must not
	// resurrect the permit nor reopen the valve.
	result.Revision = 2
	if _, err := book.ApplyResult(result, now); !errors.Is(err, ErrLineIsolated) {
		t.Fatalf("expected ErrLineIsolated after revoke, got %v", err)
	}
	if valveOpen(valves) {
		t.Fatal("valve must not reopen after isolation")
	}
}
