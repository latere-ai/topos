// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/rpc"
)

func TestServeCancelIdle(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rpc.Serve(ctx, s, &errProvider{}) }()
	cancel()
	requireCanceled(t, done, context.Canceled)
}

func TestServeCancelBlockedResponse(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rpc.Serve(ctx, s, &errProvider{}) }()
	if err := json.NewEncoder(c).Encode(map[string]any{"method": "HealthCheck"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	requireCanceled(t, done, context.Canceled)
}

type blockingProvider struct {
	sandbox.Provider
	started, stopped chan struct{}
}

func (p *blockingProvider) Exec(ctx context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
	close(p.started)
	<-ctx.Done()
	close(p.stopped)
	return sandbox.ExecResult{}, ctx.Err()
}

func (p *blockingProvider) StreamExec(ctx context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecStream, error) {
	return &blockingStream{ctx: ctx, p: p}, nil
}

type blockingStream struct {
	ctx context.Context
	p   *blockingProvider
}

func (s *blockingStream) Recv() ([]byte, error) {
	close(s.p.started)
	<-s.ctx.Done()
	close(s.p.stopped)
	return nil, s.ctx.Err()
}
func (*blockingStream) Result() sandbox.ExecResult { return sandbox.ExecResult{} }
func (*blockingStream) Close() error               { return nil }

func TestServePeerDisconnectCancelsWork(t *testing.T) {
	for _, method := range []string{"Exec", "StreamExec"} {
		t.Run(method, func(t *testing.T) {
			c, s := net.Pipe()
			defer c.Close()
			defer s.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p := &blockingProvider{started: make(chan struct{}), stopped: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- rpc.Serve(ctx, s, p) }()
			if err := json.NewEncoder(c).Encode(map[string]any{"method": method}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.started:
			case <-time.After(time.Second):
				t.Fatal("provider did not start")
			}
			_ = c.Close()
			select {
			case <-p.stopped:
			case <-time.After(250 * time.Millisecond):
				t.Fatal("peer disconnected but provider context remains active")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("clean disconnect = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("server remained blocked after peer disconnected")
			}
		})
	}
}

func TestServeMalformedRequest(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	done := make(chan error, 1)
	go func() { done <- rpc.Serve(t.Context(), s, &errProvider{}) }()
	if _, err := c.Write([]byte("!\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Fatalf("malformed request = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed request did not terminate server")
	}
}
