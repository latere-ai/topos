// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"
)

// ErrKeysUnavailable is an authorizer whose key routes did not answer,
// or answered a server error.
var ErrKeysUnavailable = errors.New("credentials: the authorizer's session key routes did not answer")

// Refused is a refusal of the key routes, such as session_unknown for a
// session the authorizer has no record of, with its code.
type Refused struct {
	Status  int
	Code    string
	Message string
}

func (e *Refused) Error() string {
	return fmt.Sprintf("credentials: the authorizer refused the session key with %d %s: %s", e.Status, e.Code, e.Message)
}

// KeyRoutes is the client of the authorizer's session key routes,
// TOPOS_SESSION_KEYS_URL and TOPOS_SESSION_KEYS_TOKEN: a PUT of
// <URL>/<session>/keys/<workload> with the SHA-256 of the value, which
// the authorizer registers with Lux and answers with the key's expiry.
type KeyRoutes struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Put registers or renews the session's key for the workload.
func (k *KeyRoutes) Put(ctx context.Context, sessionID, workload, hash string) (time.Time, error) {
	body, err := json.Marshal(map[string]string{"value_sha256": hash})
	if err != nil {
		return time.Time{}, err
	}
	u := strings.TrimRight(k.URL, "/") + "/" + url.PathEscape(sessionID) + "/keys/" + url.PathEscape(workload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrKeysUnavailable, err)
	}
	// The body's close error is the connection's, and no answer is lost.
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrKeysUnavailable, err)
	}
	switch {
	case resp.StatusCode >= 500:
		return time.Time{}, fmt.Errorf("%w: it answered %d", ErrKeysUnavailable, resp.StatusCode)
	case resp.StatusCode >= 300:
		var env httpjson.ErrorEnvelope
		if json.Unmarshal(raw, &env) != nil || env.Error.Code == "" {
			return time.Time{}, &Refused{Status: resp.StatusCode, Code: "unknown", Message: strings.TrimSpace(string(raw))}
		}
		return time.Time{}, &Refused{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	var out struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ExpiresAt.IsZero() {
		return time.Time{}, fmt.Errorf("credentials: the session key's answer names no expiry: %s", strings.TrimSpace(string(raw)))
	}
	return out.ExpiresAt, nil
}
