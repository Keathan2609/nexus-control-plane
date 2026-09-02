// Package httpx holds the HTTP response contract: how NEXUS returns data, how it
// returns errors, and how it pages.
//
// The shapes are defined once, here, because an API whose error body differs between
// handlers forces every client to special-case them. The CLI and the console both parse
// exactly one error shape.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// ErrorCode is a stable, machine-readable classification. Clients switch on this;
// Message is for humans and may change.
type ErrorCode string

const (
	CodeBadRequest    ErrorCode = "bad_request"
	CodeValidation    ErrorCode = "validation_failed"
	CodeUnauthorised  ErrorCode = "unauthorised"
	CodeForbidden     ErrorCode = "forbidden"
	CodeNotFound      ErrorCode = "not_found"
	CodeConflict      ErrorCode = "conflict"
	CodeRateLimited   ErrorCode = "rate_limited"
	CodeInternal      ErrorCode = "internal_error"
	CodeUnavailable   ErrorCode = "unavailable"
	CodeInvalidState  ErrorCode = "invalid_state"
	CodeUnprocessable ErrorCode = "unprocessable"
)

// FieldError names a specific invalid input.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ErrorBody is the single error shape the API returns.
type ErrorBody struct {
	Code      ErrorCode    `json:"code"`
	Message   string       `json:"message"`
	Fields    []FieldError `json:"fields,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
}

type errorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// APIError is an error that carries its own HTTP status and code, so handlers return
// errors rather than writing responses and the middleware renders them consistently.
type APIError struct {
	Status  int
	Code    ErrorCode
	Message string
	Fields  []FieldError
	// Cause is logged but never returned to the client.
	Cause error
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *APIError) Unwrap() error { return e.Cause }

func BadRequest(msg string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: CodeBadRequest, Message: msg}
}

func Validation(msg string, fields ...FieldError) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidation, Message: msg, Fields: fields}
}

func Unauthorised(msg string) *APIError {
	return &APIError{Status: http.StatusUnauthorized, Code: CodeUnauthorised, Message: msg}
}

func Forbidden(msg string) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: CodeForbidden, Message: msg}
}

func NotFound(resource string) *APIError {
	return &APIError{Status: http.StatusNotFound, Code: CodeNotFound,
		Message: fmt.Sprintf("%s not found", resource)}
}

func Conflict(msg string) *APIError {
	return &APIError{Status: http.StatusConflict, Code: CodeConflict, Message: msg}
}

func InvalidState(msg string) *APIError {
	return &APIError{Status: http.StatusConflict, Code: CodeInvalidState, Message: msg}
}

// Internal wraps an unexpected failure. The cause is logged; the client is told nothing
// beyond that something went wrong, because internal error text leaks schema and
// topology detail.
func Internal(cause error) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Code: CodeInternal,
		Message: "an internal error occurred", Cause: cause}
}

// JSON writes a successful response.
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so there is nothing useful to do but stop.
		return
	}
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error renders an error using the single error shape.
func Error(w http.ResponseWriter, requestID string, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = Internal(err)
	}
	JSON(w, apiErr.Status, errorEnvelope{Error: ErrorBody{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Fields:    apiErr.Fields,
		RequestID: requestID,
	}})
}

// DecodeJSON reads a request body into v, rejecting unknown fields.
//
// Rejecting unknown fields turns a client typo into a 400 naming the field rather than
// a silently ignored value and a support question about why a setting had no effect.
func DecodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return BadRequest("a request body is required")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return BadRequest("request body is not valid JSON: " + err.Error())
	}
	return nil
}

// --- pagination ---------------------------------------------------------------

// Page is the envelope every list endpoint returns.
//
// Keyset pagination, not offset. Offset paging over a table that is actively being
// written to skips and repeats rows as new entries shift the window, which for a run
// list that gains entries continuously is not an edge case but the normal experience.
type Page[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

func NewPage[T any](items []T, limit int, cursorOf func(T) string) Page[T] {
	if items == nil {
		items = []T{}
	}
	p := Page[T]{Data: items}
	if len(items) > limit {
		p.Data = items[:limit]
		p.HasMore = true
		p.NextCursor = cursorOf(p.Data[len(p.Data)-1])
	}
	return p
}

// Limit reads a bounded page size from the query string.
func Limit(r *http.Request, def, max int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func Cursor(r *http.Request) string { return r.URL.Query().Get("cursor") }

// Handler is a handler that returns an error, so handlers can `return err` instead of
// writing a response and remembering to return.
type Handler func(w http.ResponseWriter, r *http.Request) error

// Wrap adapts a Handler to http.Handler, rendering any error it returns.
func Wrap(h Handler, requestIDOf func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			Error(w, requestIDOf(r), err)
		}
	}
}
