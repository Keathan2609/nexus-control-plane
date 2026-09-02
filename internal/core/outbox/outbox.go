// Package outbox implements the transactional outbox: the mechanism that stops a state
// change and the event announcing it from ever diverging.
//
// The problem it solves is unavoidable in any system that owns a database and a message
// broker. Publish inside the transaction and a broker timeout rolls back work that
// really happened; publish after committing and a crash in between loses the event
// permanently, with nothing anywhere recording that it was owed. Neither failure is
// detectable after the fact, which is what makes them worse than an outage.
//
// Writing the event into the database in the same transaction as the state change
// collapses the two writes into one atomic act. A relay then moves rows to NATS at its
// leisure, retrying until it succeeds. The cost is that delivery becomes at-least-once
// rather than exactly-once, which consumers handle with the processed_events guard.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

// Publisher is the transport the relay drains into. Kept as an interface so the relay
// is testable without a broker, and so NATS is not welded into the core.
type Publisher interface {
	Publish(ctx context.Context, e events.Envelope) error
}

type PublisherFunc func(ctx context.Context, e events.Envelope) error

func (f PublisherFunc) Publish(ctx context.Context, e events.Envelope) error { return f(ctx, e) }

// Write records an event inside the caller's transaction.
//
// Taking a pgx.Tx rather than a pool is the whole point: this function cannot be called
// outside a transaction, so it is not possible to write an event that is not atomic
// with something else.
func Write(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO outbox
		   (event_id, event_type, event_version, aggregate_type, aggregate_id,
		    correlation_id, causation_id, payload, occurred_at)
		 VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)`,
		e.ID, string(e.Type), e.Version, e.AggregateType, e.AggregateID,
		e.CorrelationID, e.CausationID, e.Payload, e.OccurredAt)
	if err != nil {
		return fmt.Errorf("write outbox event %s: %w", e.Type, err)
	}
	return nil
}

// WriteAll records several events in one transaction.
func WriteAll(ctx context.Context, tx pgx.Tx, envelopes ...events.Envelope) error {
	for _, e := range envelopes {
		if err := Write(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

// Relay moves committed outbox rows onto the transport.
type Relay struct {
	db  *database.DB
	pub Publisher

	// BaseBackoff and MaxBackoff bound the exponential retry applied to rows whose
	// publish failed.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

func NewRelay(db *database.DB, pub Publisher) *Relay {
	return &Relay{
		db:          db,
		pub:         pub,
		BaseBackoff: 2 * time.Second,
		MaxBackoff:  5 * time.Minute,
	}
}

// Drain publishes up to limit pending rows and reports how many were published.
//
// Rows are selected FOR UPDATE SKIP LOCKED so several API replicas can each run a relay
// without publishing the same event twice — the same primitive the job scheduler uses,
// applied to a different queue.
//
// A publish failure is recorded against the row and retried later; it is never returned
// as an error from Drain, because one unreachable subject must not stop the rest of the
// backlog from draining.
func (r *Relay) Drain(ctx context.Context, limit int) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin relay: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, event_id, event_type, event_version, aggregate_type, aggregate_id,
		        correlation_id, COALESCE(causation_id,''), payload, occurred_at, attempts
		   FROM outbox
		  WHERE published_at IS NULL
		    AND next_attempt_at <= now()
		  ORDER BY id
		  FOR UPDATE SKIP LOCKED
		  LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("select pending: %w", err)
	}

	type pending struct {
		rowID    int64
		attempts int
		env      events.Envelope
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(
			&p.rowID, &p.env.ID, &p.env.Type, &p.env.Version, &p.env.AggregateType,
			&p.env.AggregateID, &p.env.CorrelationID, &p.env.CausationID,
			&p.env.Payload, &p.env.OccurredAt, &p.attempts,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan pending: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate pending: %w", err)
	}

	published := 0
	for _, p := range batch {
		if err := r.pub.Publish(ctx, p.env); err != nil {
			if _, uerr := tx.Exec(ctx,
				`UPDATE outbox
				    SET attempts = attempts + 1,
				        last_error = $2,
				        next_attempt_at = now() + make_interval(secs => $3)
				  WHERE id = $1`,
				p.rowID, err.Error(), r.backoffFor(p.attempts+1).Seconds()); uerr != nil {
				return published, fmt.Errorf("record publish failure: %w", uerr)
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE outbox SET published_at = now(), attempts = attempts + 1, last_error = NULL
			  WHERE id = $1`, p.rowID); err != nil {
			return published, fmt.Errorf("mark published: %w", err)
		}
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit relay: %w", err)
	}
	return published, nil
}

// backoffFor is exponential with a ceiling. No jitter here: unlike job retries, outbox
// rows are drained by a small number of relays reading a shared queue with SKIP LOCKED,
// so there is no thundering herd to spread out.
func (r *Relay) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(float64(r.BaseBackoff) * math.Pow(2, float64(attempt-1)))
	if d > r.MaxBackoff || d <= 0 {
		return r.MaxBackoff
	}
	return d
}

// Run drains continuously until the context is cancelled.
//
// The interval is a floor, not a guarantee of latency: whenever a drain fills its batch
// the loop immediately drains again, so a backlog is cleared as fast as the transport
// allows rather than at one batch per tick.
func (r *Relay) Run(ctx context.Context, interval time.Duration, batch int) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for {
				n, err := r.Drain(ctx, batch)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return err
					}
					break // transient; the next tick retries
				}
				if n < batch {
					break
				}
			}
		}
	}
}

// Prune deletes published rows older than the retention window, keeping the table and
// its indexes bounded. Called by the reconciler.
func (r *Relay) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM outbox
		  WHERE published_at IS NOT NULL
		    AND published_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("prune outbox: %w", err)
	}
	return tag.RowsAffected(), nil
}
