// Package audit records who did what to the control plane.
//
// The trail is append-only: the application role is granted INSERT and SELECT on
// audit_events but not UPDATE or DELETE, so immutability is enforced by the database
// rather than promised by convention. Nothing in this package offers a way to amend or
// remove a record, and that omission is the point.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Keathan2609/nexus/internal/auth"
	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/platform/database"
)

// Action names a recorded operation. These strings appear in the console and in
// exported records, so they are part of the product surface.
type Action string

const (
	ProjectCreated      Action = "project.created"
	ProjectUpdated      Action = "project.updated"
	ProjectDeleted      Action = "project.deleted"
	RepositoryConnected Action = "repository.connected"

	PipelineTriggered Action = "pipeline.triggered"
	PipelineCancelled Action = "pipeline.cancelled"

	DeploymentRequested Action = "deployment.requested"
	RollbackRequested   Action = "rollback.requested"

	EnvironmentCreated   Action = "environment.created"
	EnvironmentDestroyed Action = "environment.destroyed"

	SecretReferenceChanged Action = "secret_reference.changed"
	RoleChanged            Action = "role.changed"
	UserLoggedIn           Action = "user.logged_in"
	TokenIssued            Action = "token.issued"
)

type Entry struct {
	ID           domain.ID      `json:"id"`
	ActorID      domain.ID      `json:"actor_id,omitempty"`
	ActorType    string         `json:"actor_type"`
	ActorLabel   string         `json:"actor_label"`
	Action       Action         `json:"action"`
	ResourceType string         `json:"resource_type"`
	ResourceID   string         `json:"resource_id"`
	ProjectID    domain.ID      `json:"project_id,omitempty"`
	Correlation  string         `json:"correlation_id,omitempty"`
	Metadata     map[string]any `json:"metadata"`
	OccurredAt   time.Time      `json:"occurred_at"`
}

type Recorder struct{ db *database.DB }

func NewRecorder(db *database.DB) *Recorder { return &Recorder{db: db} }

type Params struct {
	Principal     *auth.Principal
	Action        Action
	ResourceType  string
	ResourceID    string
	ProjectID     domain.ID
	CorrelationID string
	IPAddress     string
	Metadata      map[string]any
}

// Record writes an audit entry outside any caller transaction.
//
// Use RecordTx instead whenever the action being audited is itself a database change:
// an audit trail that can disagree with the state it describes is worse than none, and
// the only way to prevent that is for both writes to share a transaction.
func (r *Recorder) Record(ctx context.Context, p Params) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin audit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := RecordTx(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordTx writes an audit entry inside the caller's transaction.
func RecordTx(ctx context.Context, tx pgx.Tx, p Params) error {
	if p.Principal == nil {
		p.Principal = &auth.Principal{}
	}
	meta := p.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	// Metadata is caller-supplied and lands in a queryable column, so anything that
	// looks like a credential is dropped rather than recorded.
	for _, k := range []string{"password", "token", "secret", "authorization", "api_key"} {
		delete(meta, k)
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}

	label := p.Principal.Email
	if label == "" {
		label = "system"
	}

	var ip any
	if p.IPAddress != "" {
		ip = p.IPAddress
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO audit_events
		   (id, organisation_id, actor_id, actor_type, actor_label, action,
		    resource_type, resource_id, project_id, correlation_id, ip_address, metadata)
		 VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12)`,
		domain.NewID(domain.PrefixAudit), p.Principal.OrganisationID, p.Principal.UserID,
		p.Principal.ActorType(), label, string(p.Action), p.ResourceType, p.ResourceID,
		p.ProjectID, p.CorrelationID, ip, blob)
	if err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

type ListParams struct {
	OrganisationID domain.ID
	ProjectID      domain.ID
	Action         string
	ResourceID     string
	Limit          int
	// Cursor is an audit id; results are those older than it. Keyset pagination rather
	// than OFFSET, so paging stays correct while new entries arrive.
	Cursor string
}

func (r *Recorder) List(ctx context.Context, p ListParams) ([]Entry, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	rows, err := r.db.Query(ctx,
		`SELECT id, COALESCE(actor_id,''), actor_type, actor_label, action,
		        resource_type, resource_id, COALESCE(project_id,''), correlation_id,
		        metadata, occurred_at
		   FROM audit_events
		  WHERE organisation_id = $1
		    AND ($2 = '' OR project_id = $2)
		    AND ($3 = '' OR action = $3)
		    AND ($4 = '' OR resource_id = $4)
		    AND ($5 = '' OR id < $5)
		  ORDER BY id DESC
		  LIMIT $6`,
		p.OrganisationID, p.ProjectID, p.Action, p.ResourceID, p.Cursor, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var meta []byte
		if err := rows.Scan(&e.ID, &e.ActorID, &e.ActorType, &e.ActorLabel, &e.Action,
			&e.ResourceType, &e.ResourceID, &e.ProjectID, &e.Correlation, &meta,
			&e.OccurredAt); err != nil {
			return nil, err
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &e.Metadata)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
