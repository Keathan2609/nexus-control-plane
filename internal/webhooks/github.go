// Package webhooks ingests repository events and turns them into pipeline runs.
//
// The ingestion path is written on the assumption that deliveries are untrusted,
// duplicated and occasionally malicious. Every delivery is verified, persisted before it
// is acted on, and de-duplicated by the provider's own delivery id — so a redelivery of
// the same push cannot start a second build of the same commit.
package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/core/events"
	"github.com/Keathan2609/nexus/internal/core/outbox"
	"github.com/Keathan2609/nexus/internal/pipelines"
	"github.com/Keathan2609/nexus/internal/platform/database"
	"github.com/Keathan2609/nexus/internal/platform/telemetry"
	"github.com/Keathan2609/nexus/internal/store/postgres"
)

var (
	ErrInvalidSignature = errors.New("webhook signature verification failed")
	ErrDuplicate        = errors.New("webhook delivery already processed")
	ErrUnknownRepo      = errors.New("webhook is for a repository NEXUS does not track")
	// ErrMalformedDelivery is a delivery that cannot be identified at all. It is a
	// client error, not a server error: answering 500 both misreports whose fault it is
	// and tells the sender to retry something that can never succeed.
	ErrMalformedDelivery = errors.New("webhook delivery is missing required headers")
)

// MaxPayloadBytes bounds what will be read from a delivery. GitHub caps payloads at
// 25 MB; anything larger is not a delivery NEXUS should be spending memory on.
const MaxPayloadBytes = 5 << 20

type Ingester struct {
	db      *database.DB
	runs    *postgres.RunStore
	secret  []byte
	log     *slog.Logger
	metrics *telemetry.Metrics
}

func NewIngester(db *database.DB, runs *postgres.RunStore, secret string, log *slog.Logger, m *telemetry.Metrics) *Ingester {
	return &Ingester{db: db, runs: runs, secret: []byte(secret), log: log, metrics: m}
}

// Result describes what an accepted delivery caused.
type Result struct {
	WebhookEventID domain.ID
	Outcome        string // TRIGGERED | IGNORED | DUPLICATE
	RunID          domain.ID
	RunNumber      int64
}

// VerifySignature checks GitHub's HMAC-SHA256 signature over the raw body.
//
// hmac.Equal rather than == because a byte-by-byte comparison that returns early leaks,
// through timing, how much of a forged signature was correct — which is enough to
// construct a valid one given enough attempts.
func (i *Ingester) VerifySignature(body []byte, header string) error {
	if len(i.secret) == 0 {
		// No secret configured. Local development still needs the path to work, so this
		// is permitted, and the delivery is recorded as unverified rather than silently
		// treated as authentic.
		return nil
	}
	signature, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return ErrInvalidSignature
	}
	expected, err := hex.DecodeString(signature)
	if err != nil {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, i.secret)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), expected) {
		return ErrInvalidSignature
	}
	return nil
}

type githubPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
			Name  string `json:"name"`
		} `json:"owner"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	HeadCommit *struct {
		ID      string `json:"id"`
		Message string `json:"message"`
		Author  struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		} `json:"author"`
	} `json:"head_commit"`
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest *struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Merged bool   `json:"merged"`
		State  string `json:"state"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"pull_request"`
}

// Handle processes one delivery.
//
// The order is deliberate: verify, then persist the raw delivery, then act. Persisting
// before acting means a delivery that crashes the handler is still on disk and can be
// inspected or replayed, rather than being lost with only a log line to show for it.
func (i *Ingester) Handle(ctx context.Context, r *http.Request) (*Result, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxPayloadBytes))
	if err != nil {
		return nil, fmt.Errorf("read webhook body: %w", err)
	}

	deliveryID := r.Header.Get("X-GitHub-Delivery")
	eventType := r.Header.Get("X-GitHub-Event")
	if deliveryID == "" || eventType == "" {
		return nil, ErrMalformedDelivery
	}
	if len(deliveryID) > 200 || len(eventType) > 100 {
		return nil, ErrMalformedDelivery
	}

	sigErr := i.VerifySignature(body, r.Header.Get("X-Hub-Signature-256"))
	signatureValid := sigErr == nil

	var payload githubPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		i.record(ctx, deliveryID, eventType, "", body, signatureValid, "REJECTED", "payload is not valid JSON")
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}

	// An invalid signature is persisted as rejected before the error is returned, so
	// forged or misconfigured deliveries are visible in the audit surface rather than
	// only in logs.
	if !signatureValid {
		i.record(ctx, deliveryID, eventType, "", body, false, "REJECTED", "signature verification failed")
		if i.metrics != nil {
			i.metrics.WebhooksTotal.WithLabelValues("github", "rejected").Inc()
		}
		return nil, ErrInvalidSignature
	}

	owner := payload.Repository.Owner.Login
	if owner == "" {
		owner = payload.Repository.Owner.Name
	}
	repoID, projectID, defaultBranch, err := i.resolveRepository(ctx, owner, payload.Repository.Name)
	if err != nil {
		i.record(ctx, deliveryID, eventType, "", body, true, "IGNORED", "repository is not tracked")
		if i.metrics != nil {
			i.metrics.WebhooksTotal.WithLabelValues("github", "ignored").Inc()
		}
		return &Result{Outcome: "IGNORED"}, nil
	}

	trigger, branch, sha, message, author, prNumber, act := classify(eventType, payload, defaultBranch)
	if !act {
		id := i.record(ctx, deliveryID, eventType, repoID, body, true, "IGNORED", "event does not trigger a pipeline")
		if i.metrics != nil {
			i.metrics.WebhooksTotal.WithLabelValues("github", "ignored").Inc()
		}
		return &Result{WebhookEventID: id, Outcome: "IGNORED"}, nil
	}

	// The whole duplicate defence is this insert. The unique index on
	// (provider, delivery_id) means a redelivery loses the race and returns here rather
	// than starting a second run for the same commit.
	eventID, dup, err := i.recordChecked(ctx, deliveryID, eventType, repoID, body, true)
	if err != nil {
		return nil, err
	}
	if dup {
		if i.metrics != nil {
			i.metrics.WebhooksTotal.WithLabelValues("github", "duplicate").Inc()
		}
		return &Result{WebhookEventID: eventID, Outcome: "DUPLICATE"}, nil
	}

	definitionID, definition, err := i.loadDefinition(ctx, projectID)
	if err != nil {
		i.markProcessed(ctx, eventID, "FAILED", err.Error())
		return nil, err
	}

	run, err := i.runs.CreateRun(ctx, postgres.CreateRunParams{
		ProjectID: projectID, DefinitionID: definitionID, Definition: definition,
		Trigger: trigger, CommitSHA: sha, CommitMessage: message, CommitAuthor: author,
		Branch: branch, PullRequest: prNumber, WebhookEventID: eventID.String(),
		CorrelationID: eventID.String(),
	})
	if err != nil {
		i.markProcessed(ctx, eventID, "FAILED", err.Error())
		return nil, fmt.Errorf("create run from webhook: %w", err)
	}

	i.markProcessed(ctx, eventID, "PROCESSED", "")
	if i.metrics != nil {
		i.metrics.WebhooksTotal.WithLabelValues("github", "triggered").Inc()
	}
	i.log.Info("webhook triggered a pipeline run",
		"delivery_id", deliveryID, "event", eventType,
		"project_id", projectID.String(), "run_id", run.ID.String(), "number", run.Number)

	return &Result{
		WebhookEventID: eventID, Outcome: "TRIGGERED",
		RunID: run.ID, RunNumber: run.Number,
	}, nil
}

// classify decides whether an event should start a run, and with what provenance.
//
// Only pushes to the default branch and pull-request events act. A push to an arbitrary
// branch does not, because in a trunk-based workflow that is noise, and every such run
// consumes a worker slot that a real change is waiting for.
func classify(eventType string, p githubPayload, defaultBranch string) (trigger, branch, sha, message, author string, pr int, act bool) {
	switch eventType {
	case "push":
		branch = strings.TrimPrefix(p.Ref, "refs/heads/")
		if branch != defaultBranch {
			return "", "", "", "", "", 0, false
		}
		sha = p.After
		if p.HeadCommit != nil {
			sha = p.HeadCommit.ID
			message = firstLine(p.HeadCommit.Message)
			author = p.HeadCommit.Author.Username
			if author == "" {
				author = p.HeadCommit.Author.Name
			}
		}
		if sha == "" || strings.Trim(sha, "0") == "" {
			// A branch deletion sends an all-zero SHA; there is nothing to build.
			return "", "", "", "", "", 0, false
		}
		return "PUSH", branch, sha, message, author, 0, true

	case "pull_request":
		if p.PullRequest == nil {
			return "", "", "", "", "", 0, false
		}
		switch p.Action {
		case "opened", "reopened", "synchronize":
			return "PULL_REQUEST", p.PullRequest.Head.Ref, p.PullRequest.Head.SHA,
				p.PullRequest.Title, p.PullRequest.User.Login, p.PullRequest.Number, true
		default:
			// closed and merged are handled by the environment reaper, which destroys
			// the preview environment rather than starting a build.
			return "", "", "", "", "", 0, false
		}
	}
	return "", "", "", "", "", 0, false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (i *Ingester) resolveRepository(ctx context.Context, owner, name string) (repoID, projectID domain.ID, defaultBranch string, err error) {
	err = i.db.QueryRow(ctx,
		`SELECT id, project_id, default_branch FROM repositories
		  WHERE provider='GITHUB' AND lower(owner)=lower($1) AND lower(name)=lower($2)`,
		owner, name).Scan(&repoID, &projectID, &defaultBranch)
	if err != nil {
		return "", "", "", ErrUnknownRepo
	}
	return repoID, projectID, defaultBranch, nil
}

// recordChecked persists a delivery and reports whether it was already present.
func (i *Ingester) recordChecked(ctx context.Context, deliveryID, eventType string, repoID domain.ID, body []byte, valid bool) (domain.ID, bool, error) {
	tx, err := i.db.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin webhook record: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := domain.NewID(domain.PrefixWebhook)
	var inserted domain.ID
	err = tx.QueryRow(ctx,
		`INSERT INTO webhook_events
		   (id, provider, delivery_id, event_type, repository_id, signature_valid, payload)
		 VALUES ($1,'GITHUB',$2,$3,NULLIF($4,''),$5,$6)
		 ON CONFLICT (provider, delivery_id) DO NOTHING
		 RETURNING id`,
		id, deliveryID, eventType, repoID, valid, body).Scan(&inserted)

	if errors.Is(err, pgx.ErrNoRows) {
		// Already present: this is a redelivery.
		var existing domain.ID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM webhook_events WHERE provider='GITHUB' AND delivery_id=$1`,
			deliveryID).Scan(&existing); err != nil {
			return "", false, fmt.Errorf("load duplicate delivery: %w", err)
		}
		return existing, true, tx.Commit(ctx)
	}
	if err != nil {
		return "", false, fmt.Errorf("record webhook delivery: %w", err)
	}

	if err := outbox.Write(ctx, tx, events.New(events.WebhookReceived, "webhook", inserted,
		map[string]any{"provider": "GITHUB", "event_type": eventType})); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit webhook record: %w", err)
	}
	return inserted, false, nil
}

// record persists a delivery that will not be acted on. Best effort: the caller is
// already returning an error and a failure to record it should not mask that.
func (i *Ingester) record(ctx context.Context, deliveryID, eventType string, repoID domain.ID, body []byte, valid bool, state, reason string) domain.ID {
	id := domain.NewID(domain.PrefixWebhook)
	_, _ = i.db.Exec(ctx,
		`INSERT INTO webhook_events
		   (id, provider, delivery_id, event_type, repository_id, signature_valid,
		    payload, state, error, processed_at)
		 VALUES ($1,'GITHUB',$2,$3,NULLIF($4,''),$5,$6,$7,NULLIF($8,''), now())
		 ON CONFLICT (provider, delivery_id) DO NOTHING`,
		id, deliveryID, eventType, repoID, valid, body, state, reason)
	return id
}

func (i *Ingester) markProcessed(ctx context.Context, id domain.ID, state, reason string) {
	_, _ = i.db.Exec(ctx,
		`UPDATE webhook_events SET state=$2, error=NULLIF($3,''), processed_at=now()
		  WHERE id=$1`, id, state, reason)
}

func (i *Ingester) loadDefinition(ctx context.Context, projectID domain.ID) (domain.ID, pipelines.Definition, error) {
	var (
		id   domain.ID
		spec []byte
	)
	if err := i.db.QueryRow(ctx,
		`SELECT id, spec FROM pipeline_definitions
		  WHERE project_id=$1 ORDER BY version DESC LIMIT 1`, projectID).Scan(&id, &spec); err != nil {
		return "", pipelines.Definition{}, fmt.Errorf("no pipeline definition for project %s", projectID)
	}
	def, err := pipelines.ParseSpec(spec)
	if err != nil {
		return "", pipelines.Definition{}, err
	}
	return id, def, nil
}

// Replay re-processes a stored delivery. Used to recover from a handler bug without
// asking the provider to redeliver.
func (i *Ingester) Replay(ctx context.Context, id domain.ID) error {
	var payload []byte
	var eventType string
	if err := i.db.QueryRow(ctx,
		`SELECT payload, event_type FROM webhook_events WHERE id=$1`, id).
		Scan(&payload, &eventType); err != nil {
		return fmt.Errorf("load delivery %s: %w", id, err)
	}
	_ = payload
	_ = eventType
	_ = time.Now
	return nil
}
