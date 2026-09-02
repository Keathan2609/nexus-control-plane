// Package events defines the event vocabulary of the control plane and the envelope
// every event travels in.
//
// Events are notifications, not state. Postgres remains the system of record; an event
// says "this happened, go look" rather than carrying authoritative data. That choice is
// what allows NATS to be lost entirely without losing work — see docs/events.md.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Keathan2609/nexus/internal/core/domain"
)

// Type is the routing key of an event. The dotted form maps directly onto NATS subjects
// (`nexus.pipeline.queued`), so adding an event type does not require touching any
// subscription wiring.
type Type string

const (
	ProjectCreated      Type = "project.created"
	RepositoryConnected Type = "repository.connected"

	WebhookReceived Type = "webhook.received"

	PipelineQueued    Type = "pipeline.queued"
	PipelineStarted   Type = "pipeline.started"
	PipelineCompleted Type = "pipeline.completed"
	PipelineFailed    Type = "pipeline.failed"
	PipelineCancelled Type = "pipeline.cancelled"

	StageStarted Type = "stage.started"

	JobQueued    Type = "job.queued"
	JobStarted   Type = "job.started"
	JobCompleted Type = "job.completed"
	JobFailed    Type = "job.failed"
	JobRetrying  Type = "job.retrying"
	JobSkipped   Type = "job.skipped"

	DeploymentRequested  Type = "deployment.requested"
	DeploymentStarted    Type = "deployment.started"
	DeploymentVerifying  Type = "deployment.verifying"
	DeploymentHealthy    Type = "deployment.healthy"
	DeploymentDegraded   Type = "deployment.degraded"
	DeploymentFailed     Type = "deployment.failed"
	DeploymentRolledBack Type = "deployment.rolled_back"

	EnvironmentCreated   Type = "environment.created"
	EnvironmentDestroyed Type = "environment.destroyed"

	DriftDetected     Type = "drift.detected"
	HealthCheckFailed Type = "healthcheck.failed"
	IncidentCreated   Type = "incident.created"
)

// Subject maps an event type onto its NATS subject.
func (t Type) Subject() string { return "nexus." + string(t) }

// CurrentVersion is the schema version new envelopes are stamped with.
//
// Consumers must tolerate an envelope whose Version is lower than the one they were
// written against — during a rolling deploy, old and new producers run side by side and
// a consumer will see both.
const CurrentVersion = 1

// Envelope wraps every event with the metadata needed to trace it.
//
// CorrelationID ties every event produced by one logical operation together: a webhook
// arrives, a run is queued, eight jobs run, a deployment is requested and verified —
// all of it shares one correlation id, so a single query reconstructs the whole causal
// chain. CausationID names the specific event that led to this one, which turns that
// flat set into a tree.
type Envelope struct {
	ID            domain.ID       `json:"id"`
	Type          Type            `json:"type"`
	Version       int             `json:"version"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   domain.ID       `json:"aggregate_id"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

// New mints a root envelope: one that begins a new causal chain. Its correlation id is
// its own id, so every descendant shares it.
func New(t Type, aggregateType string, aggregateID domain.ID, payload any) Envelope {
	id := domain.NewID(domain.PrefixEvent)
	return Envelope{
		ID:            id,
		Type:          t,
		Version:       CurrentVersion,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		CorrelationID: id.String(),
		OccurredAt:    time.Now().UTC(),
		Payload:       marshalPayload(payload),
	}
}

// Caused mints an envelope descended from e, inheriting its correlation and pointing
// back at it.
func (e Envelope) Caused(t Type, aggregateType string, aggregateID domain.ID, payload any) Envelope {
	return Envelope{
		ID:            domain.NewID(domain.PrefixEvent),
		Type:          t,
		Version:       CurrentVersion,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		CorrelationID: e.CorrelationID,
		CausationID:   e.ID.String(),
		OccurredAt:    time.Now().UTC(),
		Payload:       marshalPayload(payload),
	}
}

// WithCorrelation attaches an existing correlation id, for events that begin inside a
// request that already has one.
func (e Envelope) WithCorrelation(correlationID string) Envelope {
	if correlationID != "" {
		e.CorrelationID = correlationID
	}
	return e
}

// marshalPayload guarantees Payload is always a valid JSON object.
//
// The outbox table has a CHECK constraint requiring jsonb_typeof(payload) = 'object',
// so a nil or unmarshalable payload must not become `null` — it would fail at INSERT
// time, inside somebody else's transaction, far from the cause.
func marshalPayload(payload any) json.RawMessage {
	if payload == nil {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// Losing the detail of an unserialisable payload is strictly better than
		// aborting the state change that produced it.
		return json.RawMessage(fmt.Sprintf(`{"_marshal_error":%q}`, err.Error()))
	}
	if len(b) == 0 || b[0] != '{' {
		return json.RawMessage(fmt.Sprintf(`{"value":%s}`, b))
	}
	return b
}

// Decode unmarshals the payload into v.
func (e Envelope) Decode(v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	return nil
}
