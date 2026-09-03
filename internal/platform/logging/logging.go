// Package logging provides structured logging with request-scoped context.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const loggerKey ctxKey = iota

// Field names are constants so a query like `job_id=job_01...` works across every
// component. A field spelled `jobID` in one place and `job_id` in another is a field
// you cannot search on.
const (
	FieldService       = "service"
	FieldComponent     = "component"
	FieldRequestID     = "request_id"
	FieldCorrelationID = "correlation_id"
	FieldProjectID     = "project_id"
	FieldRunID         = "pipeline_run_id"
	FieldJobID         = "job_id"
	FieldDeploymentID  = "deployment_id"
	FieldEnvironmentID = "environment_id"
	FieldWorkerID      = "worker_id"
	FieldEventType     = "event_type"
	FieldError         = "error"
)

// redactedKeys never appear in output regardless of what a caller passes.
//
// This is a backstop, not the primary defence — secrets should not reach a log call in
// the first place — but a backstop that costs one map lookup per attribute is worth
// having when the alternative is a credential in a log aggregator.
var redactedKeys = map[string]bool{
	"password": true, "password_hash": true, "token": true, "token_hash": true,
	"secret": true, "authorization": true, "api_key": true, "private_key": true,
	"webhook_secret": true, "jwt_secret": true, "database_url": true,
	"cookie": true, "set-cookie": true, "credential": true, "credentials": true,
	"passwd": true, "signature": true,
}

// redactedMarkers catch the variants an exact list cannot anticipate. Callers name
// fields to suit the code they are in — auth_token, client_secret, signing_key — and an
// allowlist of exact spellings is one plausible new name away from a credential in a
// log aggregator. Substring matching over a short list of markers is the cheap way to
// be wrong in the safe direction.
var redactedMarkers = []string{
	"password", "passwd", "secret", "token", "credential",
	"api_key", "apikey", "api-key", "private_key", "signing_key",
	"encryption_key", "cookie", "authorization", "signature",
}

// measurementSuffixes are the endings that turn a secret-sounding name into a number.
// token_count and secrets_configured are telemetry, and redacting them hides useful
// data for no benefit — which is how people end up switching redaction off.
var measurementSuffixes = []string{
	"_count", "_total", "_configured", "_seconds", "_ms", "_bytes", "_size", "_len",
}

// shouldRedact reports whether a field name looks like it carries a credential.
func shouldRedact(key string) bool {
	k := strings.ToLower(key)
	if redactedKeys[k] {
		return true
	}
	for _, suffix := range measurementSuffixes {
		if strings.HasSuffix(k, suffix) {
			return false
		}
	}
	for _, marker := range redactedMarkers {
		if strings.Contains(k, marker) {
			return true
		}
	}
	return false
}

func New(service, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(level),
		ReplaceAttr: redact,
	}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler).With(slog.String(FieldService, service))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if shouldRedact(a.Key) {
		return slog.String(a.Key, "[redacted]")
	}
	return a
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Into stores a logger on the context so handlers deeper in a request can log with the
// request's fields already attached, without threading a logger through every signature.
func Into(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// From retrieves the context logger, falling back to the default so a caller never has
// to nil-check.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
