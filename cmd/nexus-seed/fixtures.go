package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"

	"github.com/Keathan2609/nexus/internal/core/domain"
	"github.com/Keathan2609/nexus/internal/pipelines"
)

// Copy that appears in the console. Written the way commit messages and CI errors
// actually read, because placeholder text is the fastest way to make a demo look fake.

var commitMessages = []string{
	"Make payment capture idempotent on retry",
	"Return 409 rather than 500 on duplicate order",
	"Add index on ledger_entries(account_id, posted_at)",
	"Drop the unused settlement webhook handler",
	"Cache currency lookups for the duration of a request",
	"Fix off-by-one in the reconciliation window",
	"Emit capture latency as a histogram, not a gauge",
	"Reject negative amounts at the API boundary",
	"Move refund handling behind the ledger service",
	"Tighten the connection pool ceiling to 16",
	"Propagate correlation ids through the worker",
	"Retry only on 5xx, never on 4xx",
}

var failureMessages = []string{
	"exit status 1: 2 assertions failed in TestCaptureIsIdempotent",
	"exit status 1: dependency scan found 1 high severity advisory in golang.org/x/net",
	"exit status 2: connection refused dialing postgres:5432",
	"exit status 1: TestReconcileWindow: expected 42 entries, got 41",
}

type logLine struct {
	stream string
	text   string
}

// logLinesFor produces plausible output for a step. Failing steps end with the error
// that actually appears on the job row, so the log and the summary agree.
func logLinesFor(step, state string) []logLine {
	base := map[string][]logLine{
		"checkout": {
			{"stdout", "cloning github.com/acme/service"},
			{"stdout", "HEAD is now at a84fc12"},
		},
		"lint": {
			{"stdout", "golangci-lint run ./..."},
			{"stdout", "0 issues"},
		},
		"test": {
			{"stdout", "go test ./... -race"},
			{"stdout", "ok  	acme/service/internal/capture	1.204s"},
			{"stdout", "ok  	acme/service/internal/ledger	0.812s"},
		},
		"security-scan": {
			{"stdout", "govulncheck ./..."},
			{"stdout", "scanning 148 modules"},
		},
		"build": {
			{"stdout", "go build -trimpath -ldflags=-s -o bin/service"},
			{"stdout", "binary is 14.2 MB"},
		},
		"containerise": {
			{"stdout", "building image ghcr.io/acme/service"},
			{"stdout", "pushed sha256:1f4c9a..."},
		},
		"deploy-staging": {
			{"stdout", "applying desired state to staging"},
			{"stdout", "rollout started"},
		},
		"smoke-test": {
			{"stdout", "GET /healthz -> 200 in 6ms"},
			{"stdout", "GET /readyz  -> 200 in 4ms"},
		},
		"promote": {
			{"stdout", "release marked promotable"},
		},
	}

	lines := base[step]
	if lines == nil {
		lines = []logLine{{"stdout", step + ": done"}}
	}
	switch state {
	case "FAILED":
		return append(append([]logLine{}, lines[:1]...),
			logLine{"stderr", failureMessages[len(step)%len(failureMessages)]})
	case "RUNNING":
		return lines[:1]
	case "SKIPPED":
		return nil
	}
	return lines
}

func splitRepo(full string) (owner, name string) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 {
		return "acme", full
	}
	return parts[0], parts[1]
}

func randomSHA(rng *rand.Rand) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 7)
	for i := range b {
		b[i] = hex[rng.Intn(len(hex))]
	}
	return string(b)
}

func envByKind(envs []seededEnv, kind string) seededEnv {
	for _, e := range envs {
		if e.kind == kind {
			return e
		}
	}
	return seededEnv{}
}

// attemptOrZero keeps the jobs_attempts_sane constraint satisfied: a BLOCKED job has
// never been attempted, so its counter must be zero.
func attemptOrZero(state string, attempt int) int {
	if state == "BLOCKED" {
		return 0
	}
	return attempt
}

func specJSON(step pipelines.StepDef) []byte {
	b, err := json.Marshal(map[string]any{
		"command": step.Command,
		"image":   step.Image,
		"env":     step.Env,
	})
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func jsonBytes(v map[string]any) ([]byte, error) {
	if v == nil {
		v = map[string]any{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	return b, nil
}

func labelFor(actor, admin domain.ID) string {
	if actor == admin {
		return adminEmail
	}
	return devEmail
}

func reasonFor(state string, rollback bool) string {
	switch {
	case rollback:
		return "rolled back after failed health verification"
	case state == "SUPERSEDED":
		return "promoted from staging"
	default:
		return "promoted from staging"
	}
}
