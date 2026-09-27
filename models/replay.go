// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// Encoder encodes a request as its Model sends it, and names the codec
// this build encodes a dialect with, "<dialect>@<llmdialect version>".
// models/dialect's Model is one.
type Encoder interface {
	Encode(req Request) ([]byte, error)
	Codec(d ir.Dialect) string
}

// Rebuild returns the request a recorded model.request sent, built again
// from log as it stood then: the harness's half of a replay. A step it
// cannot build again returns an error wrapping ErrNotRebuilt, which the
// replay reports as skipped.
type Rebuild func(ctx context.Context, log []session.Event, step session.Event, mr session.ModelRequest) (Request, error)

// ErrNotRebuilt is a step a replay cannot build again from the log.
var ErrNotRebuilt = errors.New("models: the request cannot be built again from the log")

// The outcomes of a replayed step. A step recorded by another codec is
// CodeMismatch, codec_mismatch, and is not compared.
const (
	ReplayMatch    = "match"
	ReplayMismatch = "mismatch"
	ReplaySkipped  = "skipped"
)

// ReplayStep is the verdict on one model.request of a log.
type ReplayStep struct {
	// Seq is the model.request's sequence number.
	Seq    uint64 `json:"seq"`
	Thread string `json:"thread,omitempty"`
	// Outcome is match, mismatch, codec_mismatch or skipped.
	Outcome string `json:"outcome"`
	// Codec is the recorded codec; Build is this build's for the step's
	// dialect.
	Codec string `json:"codec,omitempty"`
	Build string `json:"build,omitempty"`
	// Recorded is the recorded request hash, Replayed the hash of the
	// request as this build encodes it again.
	Recorded string `json:"recorded,omitempty"`
	Replayed string `json:"replayed,omitempty"`
	// Detail says why a step was skipped.
	Detail string `json:"detail,omitempty"`
}

// Replay folds a log step by step (spec 007): for each model.request,
// rebuild builds the request again from the log before it, enc encodes
// it with this build's codec, and the hash of the bytes is compared with
// the one recorded. A step recorded by another codec version is
// codec_mismatch and is not compared, and one that recorded no hash,
// such as a request that failed, is skipped.
func Replay(ctx context.Context, log []session.Event, rebuild Rebuild, enc Encoder) ([]ReplayStep, error) {
	var out []ReplayStep
	for _, e := range log {
		if e.Type != session.TypeModelRequest {
			continue
		}
		step := ReplayStep{Seq: e.Seq, Thread: e.Thread}
		if e.Redacted() {
			step.Outcome, step.Detail = ReplaySkipped, "the model.request is redacted"
			out = append(out, step)
			continue
		}
		var mr session.ModelRequest
		if err := e.Decode(&mr); err != nil {
			return nil, fmt.Errorf("models: replay the model.request at %d: %w", e.Seq, err)
		}
		step.Codec, step.Build, step.Recorded = mr.Codec, enc.Codec(ir.Dialect(mr.Dialect)), mr.RequestSHA256
		switch {
		case mr.RequestSHA256 == "":
			step.Outcome, step.Detail = ReplaySkipped, "the request recorded no hash"
		case mr.Codec != step.Build:
			step.Outcome = CodeMismatch
		default:
			req, err := rebuild(ctx, log, e, mr)
			if errors.Is(err, ErrNotRebuilt) {
				step.Outcome, step.Detail = ReplaySkipped, err.Error()
				break
			}
			if err != nil {
				return nil, fmt.Errorf("models: replay the model.request at %d: %w", e.Seq, err)
			}
			body, err := enc.Encode(req)
			if err != nil {
				return nil, fmt.Errorf("models: replay the model.request at %d: %w", e.Seq, err)
			}
			sum := sha256.Sum256(body)
			step.Replayed = hex.EncodeToString(sum[:])
			step.Outcome = ReplayMatch
			if step.Replayed != step.Recorded {
				step.Outcome = ReplayMismatch
			}
		}
		out = append(out, step)
	}
	return out, nil
}
