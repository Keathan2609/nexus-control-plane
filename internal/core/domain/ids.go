package domain

import (
	"crypto/rand"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"

	"github.com/oklog/ulid/v2"
)

// Identifiers in NEXUS are prefixed ULIDs: "run_01JQ8Z3M9K...".
//
// ULID rather than UUIDv4 because the first 48 bits are a millisecond timestamp, so IDs
// sort chronologically. That matters more than it sounds: a b-tree index on a ULID
// primary key appends rather than scattering, and "ORDER BY id DESC" is a free, exact
// "most recent first" without a second index on created_at.
//
// The prefix is not decoration. An identifier that arrives in a log line, a URL or a
// support question carries its own type, so a job ID pasted where a run ID belongs is
// rejected at parse time rather than returning an empty result set.

type IDPrefix string

const (
	PrefixOrganisation IDPrefix = "org"
	PrefixUser         IDPrefix = "usr"
	PrefixTeam         IDPrefix = "team"
	PrefixProject      IDPrefix = "prj"
	PrefixRepository   IDPrefix = "repo"
	PrefixEnvironment  IDPrefix = "env"
	PrefixService      IDPrefix = "svc"
	PrefixPipeline     IDPrefix = "pipe"
	PrefixRun          IDPrefix = "run"
	PrefixJob          IDPrefix = "job"
	PrefixAttempt      IDPrefix = "att"
	PrefixDeployment   IDPrefix = "dep"
	PrefixRelease      IDPrefix = "rel"
	PrefixArtifact     IDPrefix = "art"
	PrefixWebhook      IDPrefix = "whk"
	PrefixEvent        IDPrefix = "evt"
	PrefixWorker       IDPrefix = "wkr"
	PrefixAudit        IDPrefix = "aud"
	PrefixSecretRef    IDPrefix = "sec"
	PrefixIncident     IDPrefix = "inc"
	PrefixHealthCheck  IDPrefix = "hc"
)

// ID is an opaque, prefixed, sortable identifier.
type ID string

func (id ID) String() string { return string(id) }
func (id ID) IsZero() bool   { return id == "" }

// Value and Scan let an ID be used directly as a Postgres text column.
func (id ID) Value() (driver.Value, error) { return string(id), nil }

func (id *ID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*id = ""
	case string:
		*id = ID(v)
	case []byte:
		*id = ID(v)
	default:
		return fmt.Errorf("cannot scan %T into domain.ID", src)
	}
	return nil
}

// Entropy is monotonic rather than purely random, and that is load-bearing.
//
// ULID timestamps have millisecond precision, so a burst of IDs minted inside one tick
// share a timestamp and would otherwise be ordered by their random tails — which is to
// say, arbitrarily. A monotonic source increments the random component instead,
// guaranteeing that within a millisecond the Nth ID sorts after the (N-1)th. Without
// this, "ORDER BY id" silently stops meaning "in the order they were created" precisely
// when the system is busiest.
//
// ulid.MonotonicEntropy is not safe for concurrent use, hence the mutex.
var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// NewID mints a fresh identifier for the given prefix.
func NewID(p IDPrefix) ID {
	entropyMu.Lock()
	u := ulid.MustNew(ulid.Now(), entropy)
	entropyMu.Unlock()
	return ID(string(p) + "_" + strings.ToLower(u.String()))
}

// ParseID validates that raw is well formed and carries the expected prefix. Callers
// parsing identifiers out of a URL should use this rather than a bare cast, so that a
// deployment ID in a job route fails loudly instead of quietly matching nothing.
func ParseID(p IDPrefix, raw string) (ID, error) {
	prefix := string(p) + "_"
	if !strings.HasPrefix(raw, prefix) {
		return "", fmt.Errorf("expected an identifier with prefix %q, got %q", p, raw)
	}
	body := strings.TrimPrefix(raw, prefix)
	if _, err := ulid.ParseStrict(strings.ToUpper(body)); err != nil {
		return "", fmt.Errorf("malformed identifier %q: %w", raw, err)
	}
	return ID(raw), nil
}

// HasPrefix reports whether the identifier belongs to the given type.
func (id ID) HasPrefix(p IDPrefix) bool {
	return strings.HasPrefix(string(id), string(p)+"_")
}
