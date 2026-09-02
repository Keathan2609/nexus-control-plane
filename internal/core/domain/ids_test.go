package domain_test

import (
	"strings"
	"testing"

	"github.com/Keathan2609/nexus/internal/core/domain"
)

func TestNewIDCarriesItsPrefix(t *testing.T) {
	id := domain.NewID(domain.PrefixRun)
	if !strings.HasPrefix(id.String(), "run_") {
		t.Fatalf("expected run_ prefix, got %s", id)
	}
	if !id.HasPrefix(domain.PrefixRun) {
		t.Fatal("HasPrefix should report true for its own prefix")
	}
	if id.HasPrefix(domain.PrefixJob) {
		t.Fatal("a run ID must not claim the job prefix")
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := make(map[domain.ID]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := domain.NewID(domain.PrefixJob)
		if seen[id] {
			t.Fatalf("duplicate identifier generated: %s", id)
		}
		seen[id] = true
	}
}

func TestIDsSortChronologically(t *testing.T) {
	// ULID time precision is a millisecond, so IDs minted in the same tick may tie.
	// Sortability is what matters: a later ID must never sort before an earlier one.
	first := domain.NewID(domain.PrefixRun)
	last := first
	for i := 0; i < 200; i++ {
		last = domain.NewID(domain.PrefixRun)
	}
	if last < first {
		t.Fatalf("later ID %s sorts before earlier ID %s", last, first)
	}
}

func TestParseIDRejectsWrongPrefix(t *testing.T) {
	jobID := domain.NewID(domain.PrefixJob)
	if _, err := domain.ParseID(domain.PrefixRun, jobID.String()); err == nil {
		t.Fatal("parsing a job ID as a run ID must fail")
	}
}

func TestParseIDAcceptsCorrectPrefix(t *testing.T) {
	runID := domain.NewID(domain.PrefixRun)
	got, err := domain.ParseID(domain.PrefixRun, runID.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != runID {
		t.Fatalf("expected %s, got %s", runID, got)
	}
}

func TestParseIDRejectsMalformedBody(t *testing.T) {
	if _, err := domain.ParseID(domain.PrefixRun, "run_not-a-ulid"); err == nil {
		t.Fatal("malformed ULID body must be rejected")
	}
}

func TestParseIDRejectsEmpty(t *testing.T) {
	if _, err := domain.ParseID(domain.PrefixRun, ""); err == nil {
		t.Fatal("empty identifier must be rejected")
	}
}

func TestIDScanRoundTrip(t *testing.T) {
	original := domain.NewID(domain.PrefixDeployment)
	var scanned domain.ID
	if err := scanned.Scan(original.String()); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if scanned != original {
		t.Fatalf("round trip changed the value: %s -> %s", original, scanned)
	}
}
