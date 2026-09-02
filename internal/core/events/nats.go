package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// StreamName is the single JetStream stream every control-plane event lands in.
const StreamName = "NEXUS"

// SubjectAll matches every NEXUS event.
const SubjectAll = "nexus.>"

// NATSPublisher publishes envelopes to JetStream.
//
// JetStream rather than core NATS because core NATS is fire-and-forget: a subscriber
// that is restarting when an event is published never sees it. Since NEXUS drives
// deployments off these events, "the console missed a state change because it was
// redeploying" is not an acceptable failure mode.
type NATSPublisher struct {
	js jetstream.JetStream
}

// Connect dials NATS and ensures the stream exists.
func Connect(ctx context.Context, url string) (*nats.Conn, *NATSPublisher, error) {
	nc, err := nats.Connect(url,
		nats.Name("nexus"),
		// Reconnect forever. A control plane that gives up on its transport after ten
		// attempts turns a brief broker restart into a permanent degradation requiring
		// a human.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ReconnectJitter(250*time.Millisecond, time.Second),
		nats.Timeout(5*time.Second),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connect nats: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("create jetstream context: %w", err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     StreamName,
		Subjects: []string{SubjectAll},
		// Events are notifications; Postgres holds the truth. A bounded window is
		// enough for consumers to catch up after a restart, and keeps the stream from
		// growing without limit.
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
		Storage:   jetstream.FileStorage,
		// Duplicate suppression on the event id. Belt and braces alongside the
		// consumer-side processed_events guard: this catches a relay that published,
		// failed to record the fact, and republished on the next drain.
		Duplicates: 5 * time.Minute,
	}); err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("ensure stream: %w", err)
	}

	return nc, &NATSPublisher{js: js}, nil
}

func (p *NATSPublisher) Publish(ctx context.Context, e Envelope) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal envelope %s: %w", e.ID, err)
	}

	msg := &nats.Msg{
		Subject: e.Type.Subject(),
		Data:    body,
		Header:  nats.Header{},
	}
	// Nats-Msg-Id drives JetStream's duplicate window.
	msg.Header.Set(jetstream.MsgIDHeader, e.ID.String())
	msg.Header.Set("Nexus-Correlation-Id", e.CorrelationID)
	msg.Header.Set("Nexus-Event-Type", string(e.Type))

	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", e.Type, err)
	}
	return nil
}

// Subscribe delivers events matching subjects to handler until ctx is cancelled.
//
// The consumer is durable and explicitly acked: an unacked message is redelivered, so a
// consumer that crashes mid-handler retries rather than silently dropping the event.
func Subscribe(ctx context.Context, nc *nats.Conn, durable string, subjects []string, handler func(context.Context, Envelope) error) error {
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}

	cons, err := js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:        durable,
		AckPolicy:      jetstream.AckExplicitPolicy,
		FilterSubjects: subjects,
		MaxDeliver:     5,
		AckWait:        30 * time.Second,
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
	if err != nil {
		return fmt.Errorf("create consumer %s: %w", durable, err)
	}

	_, err = cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			// A message that cannot be parsed will never parse. Redelivering it forever
			// would block nothing but would fill the logs, so it is terminated.
			_ = msg.Term()
			return
		}
		if err := handler(ctx, env); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("consume %s: %w", durable, err)
	}
	return nil
}
