//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

// beginTx starts a transaction whose rollback is registered with the test.
//
// This matters more than it looks: a transaction abandoned by t.Fatalf keeps its pooled
// connection checked out, and pgxpool.Close then blocks forever waiting for it. A
// single leaked transaction hangs the whole package rather than failing one test.
func beginTx(t *testing.T, db *database.DB) pgx.Tx {
	t.Helper()
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func outboxCount(t *testing.T, db *database.DB, aggregateID domain.ID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1`, aggregateID).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// The property the outbox exists for: an event and the state change that produced it
// are committed together or not at all. If the transaction rolls back, no event is left
// behind claiming something happened that did not.
func TestOutboxWriteRollsBackWithItsTransaction(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	ctx := context.Background()

	env := events.New(events.PipelineQueued, "pipeline_run", f.RunID, map[string]any{"number": 1})

	tx := beginTx(t, db)
	if err := outbox.Write(ctx, tx, env); err != nil {
		t.Fatalf("outbox write: %v", err)
	}
	// Abandon the transaction, exactly as a failing handler would.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if n := outboxCount(t, db, f.RunID); n != 0 {
		t.Fatalf("rolled-back transaction must leave no outbox row, found %d", n)
	}
}

func TestOutboxWriteCommitsWithItsTransaction(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	ctx := context.Background()

	env := events.New(events.PipelineQueued, "pipeline_run", f.RunID, map[string]any{"number": 1})

	tx := beginTx(t, db)
	if err := outbox.Write(ctx, tx, env); err != nil {
		t.Fatalf("outbox write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n := outboxCount(t, db, f.RunID); n != 1 {
		t.Fatalf("expected exactly one outbox row, found %d", n)
	}
}

func TestOutboxRejectsDuplicateEventID(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	ctx := context.Background()

	env := events.New(events.PipelineQueued, "pipeline_run", f.RunID, map[string]any{})

	tx := beginTx(t, db)
	if err := outbox.Write(ctx, tx, env); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Writing the same envelope again must violate the unique index on event_id.
	if err := outbox.Write(ctx, tx, env); err == nil {
		t.Fatal("writing the same event id twice must be rejected")
	}
}

// The relay drains committed rows and marks them published. Nothing may be marked
// published that was not actually handed to the publisher.
func TestRelayPublishesPendingRowsAndMarksThem(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		env := events.New(events.JobQueued, "pipeline_run", f.RunID, map[string]any{"i": i})
		tx := beginTx(t, db)
		if err := outbox.Write(ctx, tx, env); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	var published []events.Envelope
	pub := outbox.PublisherFunc(func(ctx context.Context, e events.Envelope) error {
		published = append(published, e)
		return nil
	})

	relay := outbox.NewRelay(db, pub)
	n, err := relay.Drain(ctx, 100)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n < 3 {
		t.Fatalf("expected at least 3 rows drained, got %d", n)
	}
	if len(published) < 3 {
		t.Fatalf("publisher saw %d events, expected at least 3", len(published))
	}

	var unpublished int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND published_at IS NULL`, f.RunID).Scan(&unpublished); err != nil {
		t.Fatalf("count: %v", err)
	}
	if unpublished != 0 {
		t.Fatalf("expected all rows marked published, %d remain", unpublished)
	}
}

// If the broker is down, rows must stay pending and be retried later — never dropped,
// and never marked published.
func TestRelayLeavesRowsPendingWhenPublishFails(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	ctx := context.Background()

	env := events.New(events.DeploymentRequested, "deployment", f.RunID, map[string]any{})
	tx := beginTx(t, db)
	if err := outbox.Write(ctx, tx, env); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	failing := outbox.PublisherFunc(func(ctx context.Context, e events.Envelope) error {
		return context.DeadlineExceeded
	})
	relay := outbox.NewRelay(db, failing)
	if _, err := relay.Drain(ctx, 10); err != nil {
		t.Fatalf("drain should absorb publish failures, got: %v", err)
	}

	var publishedAt *time.Time
	var attempts int
	if err := db.QueryRow(ctx,
		`SELECT published_at, attempts FROM outbox WHERE aggregate_id=$1`, f.RunID).
		Scan(&publishedAt, &attempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if publishedAt != nil {
		t.Fatal("a row whose publish failed must not be marked published")
	}
	if attempts < 1 {
		t.Fatalf("failed publish must record an attempt, got %d", attempts)
	}
}

func TestRelayBacksOffFailedRows(t *testing.T) {
	db := openDB(t)
	f := newFixture(t, db)
	ctx := context.Background()

	env := events.New(events.DeploymentFailed, "deployment", f.RunID, map[string]any{})
	tx := beginTx(t, db)
	if err := outbox.Write(ctx, tx, env); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	failing := outbox.PublisherFunc(func(ctx context.Context, e events.Envelope) error {
		return context.DeadlineExceeded
	})
	relay := outbox.NewRelay(db, failing)
	if _, err := relay.Drain(ctx, 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	var next time.Time
	if err := db.QueryRow(ctx,
		`SELECT next_attempt_at FROM outbox WHERE aggregate_id=$1`, f.RunID).Scan(&next); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !next.After(time.Now()) {
		t.Fatal("a failed publish must schedule its retry in the future")
	}

	// A second immediate drain must not pick the row up again.
	n, err := relay.Drain(ctx, 10)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if n != 0 {
		t.Fatalf("row still in backoff must not be re-attempted, drained %d", n)
	}
}

func TestEnvelopeCarriesCorrelationThroughCausation(t *testing.T) {
	parent := events.New(events.WebhookReceived, "webhook", domain.NewID(domain.PrefixWebhook), map[string]any{})
	child := parent.Caused(events.PipelineQueued, "pipeline_run", domain.NewID(domain.PrefixRun), map[string]any{})

	if child.CorrelationID != parent.CorrelationID {
		t.Fatalf("correlation must propagate: %s != %s", child.CorrelationID, parent.CorrelationID)
	}
	if child.CausationID != parent.ID.String() {
		t.Fatalf("causation must point at the parent event: %s != %s", child.CausationID, parent.ID)
	}
	if child.ID == parent.ID {
		t.Fatal("a caused event must have its own id")
	}
}

func TestEnvelopePayloadIsJSONObject(t *testing.T) {
	env := events.New(events.PipelineQueued, "pipeline_run", domain.NewID(domain.PrefixRun),
		map[string]any{"number": 7, "branch": "main"})
	var decoded map[string]any
	if err := json.Unmarshal(env.Payload, &decoded); err != nil {
		t.Fatalf("payload must be a JSON object: %v", err)
	}
	if decoded["branch"] != "main" {
		t.Fatalf("payload lost data: %v", decoded)
	}
}
