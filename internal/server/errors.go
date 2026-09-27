// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// The error codes this spec owns; the others are their specs'.
const (
	CodeInvalidRequest      = "invalid_request"
	CodeNotFound            = auth.CodeNotFound
	CodeConflict            = "conflict"
	CodeSequenceConflict    = "sequence_conflict"
	CodeIdempotencyConflict = "idempotency_conflict"
	CodePayloadTooLarge     = "payload_too_large"
	CodeRateLimited         = "rate_limited"
	CodeInternal            = "internal"
	// CodeMachineUnavailable is spec 009's: the session's machine cannot
	// be had.
	CodeMachineUnavailable = "machine_unavailable"
)

// codes is spec 015's error table: every code the API answers, its
// status, and the one sentence a person reads for it.
var codes = map[string]struct {
	status  int
	message string
}{
	CodeInvalidRequest:              {http.StatusBadRequest, "The request is not valid."},
	manifest.CodeInvalidManifest:    {http.StatusBadRequest, "The manifest is not valid."},
	manifest.CodeHoldsSecret:        {http.StatusBadRequest, "The manifest holds what looks like a secret; name a credential instead."},
	manifest.CodeUnknownReference:   {http.StatusBadRequest, "The manifest references an object that does not exist."},
	manifest.CodeUnsupportedVersion: {http.StatusBadRequest, "The manifest's apiVersion is not topos.latere.ai/v1."},
	auth.CodeUnauthenticated:        {http.StatusUnauthorized, "The request carries no token this server accepts."},
	auth.CodeForbidden:              {http.StatusForbidden, "You may not do this."},
	CodeNotFound:                    {http.StatusNotFound, "There is nothing here, or you may not see it."},
	CodeConflict:                    {http.StatusConflict, "The object's state refuses the request."},
	CodeSequenceConflict:            {http.StatusConflict, "The session moved on; read it again and retry."},
	CodeIdempotencyConflict:         {http.StatusConflict, "The idempotency key was used with another request."},
	CodePayloadTooLarge:             {http.StatusRequestEntityTooLarge, "The request body is too large."},
	CodeRateLimited:                 {http.StatusTooManyRequests, "Too many requests; wait and try again."},
	CodeMachineUnavailable:          {http.StatusUnprocessableEntity, "The session's machine is not available here."},
	auth.CodeAuthorizerUnavailable:  {http.StatusServiceUnavailable, "The authorizer did not answer; try again."},
	CodeInternal:                    {http.StatusInternalServerError, "The server failed to answer; try again."},
}

// apiError is a refusal the API renders: a code of the table and the
// developer detail, which is never the source of the message.
type apiError struct {
	code       string
	detail     string
	details    map[string]any
	retryAfter time.Duration
	err        error
}

func (e *apiError) Error() string {
	if e.err != nil {
		return e.code + ": " + e.detail + ": " + e.err.Error()
	}
	return e.code + ": " + e.detail
}

func (e *apiError) Unwrap() error { return e.err }

// refuse is a refusal with a formatted detail.
func refuse(code, format string, args ...any) *apiError {
	return &apiError{code: code, detail: fmt.Sprintf(format, args...)}
}

// classify turns any error a handler returns into the refusal it
// answers. An error of no known kind is internal, and its text stays in
// the log.
func classify(err error) *apiError {
	if e, ok := errors.AsType[*apiError](err); ok {
		return e
	}
	if e, ok := errors.AsType[*auth.Error](err); ok {
		if e.Code == auth.CodeNotFound {
			// A denied read answers exactly as a missing object does.
			return &apiError{code: CodeNotFound, err: err}
		}
		return &apiError{code: e.Code, detail: e.Message, err: err}
	}
	if e, ok := errors.AsType[*manifest.Error](err); ok {
		problems := make([]map[string]any, len(e.Problems))
		for i, p := range e.Problems {
			problems[i] = map[string]any{"document": p.Doc + 1, "path": p.Path, "detail": p.Detail}
		}
		return &apiError{code: e.Code, details: map[string]any{"problems": problems}, err: err}
	}
	if e, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return &apiError{code: CodePayloadTooLarge, detail: "at most " + strconv.FormatInt(e.Limit, 10) + " bytes", err: err}
	}
	switch {
	case errors.Is(err, session.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return &apiError{code: CodeNotFound, err: err}
	case errors.Is(err, session.ErrSequenceConflict):
		return &apiError{code: CodeSequenceConflict, err: err}
	case errors.Is(err, session.ErrLocked), errors.Is(err, session.ErrExists), errors.Is(err, store.ErrConflict):
		return &apiError{code: CodeConflict, detail: err.Error(), err: err}
	case errors.Is(err, session.ErrInvalid):
		return &apiError{code: CodeInvalidRequest, detail: err.Error(), err: err}
	}
	return &apiError{code: CodeInternal, err: err}
}

// writeError renders err as the envelope of latere.ai/x/pkg/httpjson.
func writeError(w http.ResponseWriter, log *slog.Logger, err error) {
	e := classify(err)
	row, ok := codes[e.code]
	if !ok {
		row = codes[CodeInternal]
	}
	if row.status >= http.StatusInternalServerError && log != nil {
		log.Error("request failed", "code", e.code, "err", err)
	}
	details := e.details
	if e.detail != "" && e.code != CodeNotFound {
		if details == nil {
			details = map[string]any{}
		}
		details["detail"] = e.detail
	}
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(e.retryAfter.Round(time.Second).Seconds())))
	}
	httpjson.WriteError(w, row.status, httpjson.Error{Code: e.code, Message: row.message, Details: details})
}
