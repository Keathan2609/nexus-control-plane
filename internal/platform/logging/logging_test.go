package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// capture builds a logger writing to a buffer with the real redaction handler, so these
// tests exercise the same path production logging takes.
func capture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redact})
	return slog.New(handler), &buf
}

func TestRedactsKnownSecretKeys(t *testing.T) {
	secret := "s3cr3t-value-that-must-never-appear"

	for _, key := range []string{
		"password", "password_hash", "token", "token_hash", "secret",
		"authorization", "api_key", "private_key", "webhook_secret",
		"jwt_secret", "database_url",
	} {
		log, buf := capture()
		log.Info("test", key, secret)
		if strings.Contains(buf.String(), secret) {
			t.Errorf("key %q leaked its value: %s", key, buf.String())
		}
		if !strings.Contains(buf.String(), "[redacted]") {
			t.Errorf("key %q was not marked redacted: %s", key, buf.String())
		}
	}
}

// Key names vary by caller. A backstop that only matches an exact list is one that a
// plausible name like auth_token or set-cookie walks straight past.
func TestRedactsSecretBearingKeyVariants(t *testing.T) {
	secret := "s3cr3t-value-that-must-never-appear"

	for _, key := range []string{
		"auth_token", "access_token", "refresh_token", "bearer_token",
		"cookie", "Set-Cookie", "client_secret", "secret_value",
		"user_password", "passwd", "credential", "credentials",
		"AUTHORIZATION", "Api-Key", "signing_key", "encryption_key",
		"session_token", "webhook_signature",
	} {
		log, buf := capture()
		log.Info("test", key, secret)
		if strings.Contains(buf.String(), secret) {
			t.Errorf("key %q leaked its value: %s", key, buf.String())
		}
	}
}

// Redaction must not swallow the ordinary fields that make a log useful.
func TestKeepsNonSensitiveFields(t *testing.T) {
	log, buf := capture()
	log.Info("request",
		"method", "GET",
		"path", "/api/v1/runs",
		"status", 200,
		"duration_ms", 12,
		"run_id", "run_01ABC",
		"worker_id", "wkr_1",
		"branch", "main",
		"commit_sha", "a84fc12",
	)

	out := buf.String()
	for _, want := range []string{"GET", "/api/v1/runs", "run_01ABC", "wkr_1", "main", "a84fc12"} {
		if !strings.Contains(out, want) {
			t.Errorf("redaction removed a legitimate field %q: %s", want, out)
		}
	}
	if strings.Contains(out, "[redacted]") {
		t.Errorf("nothing here should have been redacted: %s", out)
	}
}

// A field named for a token count is telemetry, not a credential. Over-redaction that
// hides useful numbers trains people to turn redaction off.
func TestDoesNotRedactMeasurementFields(t *testing.T) {
	log, buf := capture()
	log.Info("metrics", "token_count", 42, "secrets_configured", 3)

	out := buf.String()
	if !strings.Contains(out, "42") {
		t.Errorf("token_count was redacted although it is a measurement: %s", out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("secrets_configured was redacted although it is a count: %s", out)
	}
}

func TestRedactionIsCaseInsensitive(t *testing.T) {
	for _, key := range []string{"PASSWORD", "Password", "pAsSwOrD"} {
		log, buf := capture()
		log.Info("test", key, "hunter2")
		if strings.Contains(buf.String(), "hunter2") {
			t.Errorf("key %q leaked its value", key)
		}
	}
}
