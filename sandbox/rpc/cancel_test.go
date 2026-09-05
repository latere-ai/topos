// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
	"latere.ai/x/topos/sandbox/rpc"
)

func unaryCalls() map[string]func(context.Context, sandbox.Provider) error {
	return map[string]func(context.Context, sandbox.Provider) error{
		"Create": func(ctx context.Context, p sandbox.Provider) error {
			_, err := p.Create(ctx, sandbox.CreateOptions{})
			return err
		},
		"Destroy": func(ctx context.Context, p sandbox.Provider) error { return p.Destroy(ctx, "sb") },
		"Exec": func(ctx context.Context, p sandbox.Provider) error {
			_, err := p.Exec(ctx, "sb", sandbox.ExecOptions{})
			return err
		},
		"ReadFile": func(ctx context.Context, p sandbox.Provider) error {
			_, err := p.ReadFile(ctx, "sb", "file")
			return err
		},
		"WriteFile":   func(ctx context.Context, p sandbox.Provider) error { return p.WriteFile(ctx, "sb", "file", nil) },
		"ListFiles":   func(ctx context.Context, p sandbox.Provider) error { _, err := p.ListFiles(ctx, "sb", "."); return err },
		"HealthCheck": func(ctx context.Context, p sandbox.Provider) error { return p.HealthCheck(ctx, "sb") },
		"StreamExec": func(ctx context.Context, p sandbox.Provider) error {
			s, err := p.StreamExec(ctx, "sb", sandbox.ExecOptions{})
			if s != nil {
				_ = s.Close()
			}
			return err
		},
	}
}

func requireCanceled(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("call returned %v, want %v", err, want)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("call remained blocked after cancellation")
	}
}

// Every public method must stop even when the peer never reads its request.
func TestClientDeadlineDuringWrite(t *testing.T) {
	for name, call := range unaryCalls() {
		t.Run(name, func(t *testing.T) {
			c, s := net.Pipe()
			defer c.Close()
			defer s.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx, rpc.NewClient(c)) }()
			<-ctx.Done()
			requireCanceled(t, done, context.DeadlineExceeded)
		})
	}
}

func TestClientCancelDuringRead(t *testing.T) {
	for name, call := range unaryCalls() {
		t.Run(name, func(t *testing.T) {
			c, s := net.Pipe()
			defer c.Close()
			defer s.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- call(ctx, rpc.NewClient(c)) }()
			if err := s.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			var req map[string]any
			if err := json.NewDecoder(s).Decode(&req); err != nil {
				t.Fatal(err)
			}
			cancel()
			requireCanceled(t, done, context.Canceled)
		})
	}
}

func TestClientCanceledWaitDoesNotInterruptActiveCall(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	p := rpc.NewClient(c)
	active := make(chan error, 1)
	go func() { active <- p.HealthCheck(t.Context(), "sb") }()
	if err := s.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	waiting := make(chan error, 1)
	go func() { waiting <- p.HealthCheck(ctx, "sb") }()
	requireCanceled(t, waiting, context.Canceled)
	if err := json.NewEncoder(s).Encode(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := <-active; err != nil {
		t.Fatalf("active call interrupted: %v", err)
	}
}

func TestClientCancelAfterCompletedCallKeepsConnection(t *testing.T) {
	p, stop := pipeClient(t, &errProvider{})
	defer stop()
	ctx, cancel := context.WithCancel(t.Context())
	if err := p.HealthCheck(ctx, "sb"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := p.HealthCheck(t.Context(), "sb"); err != nil {
		t.Fatalf("completed call's cancellation closed connection: %v", err)
	}
	// A context canceled before acquiring the idle connection must not send a
	// request or install a callback that closes the reusable connection.
	if err := p.HealthCheck(ctx, "sb"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call = %v", err)
	}
	if err := p.HealthCheck(t.Context(), "sb"); err != nil {
		t.Fatal(err)
	}
}

func TestClientCancelStream(t *testing.T) {
	for _, operation := range []string{"Recv", "Close"} {
		t.Run(operation, func(t *testing.T) {
			c, s := net.Pipe()
			defer c.Close()
			defer s.Close()
			p := rpc.NewClient(c)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			peer := make(chan error, 1)
			go func() {
				var req map[string]any
				err := json.NewDecoder(s).Decode(&req)
				if err == nil {
					err = json.NewEncoder(s).Encode(map[string]any{"chunk": []byte("first")})
				}
				peer <- err
			}()
			stream, err := p.StreamExec(ctx, "sb", sandbox.ExecOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := <-peer; err != nil {
				t.Fatal(err)
			}
			if got, err := stream.Recv(); err != nil || string(got) != "first" {
				t.Fatalf("first chunk = %q, %v", got, err)
			}
			done := make(chan error, 1)
			go func() {
				if operation == "Close" {
					done <- stream.Close()
					return
				}
				_, err := stream.Recv()
				done <- err
			}()
			cancel()
			want := context.Canceled
			if operation == "Close" {
				want = nil
			}
			requireCanceled(t, done, want)
			_ = stream.Close()
			// The canceled stream must also release the serialization gate.
			next := make(chan error, 1)
			go func() { next <- p.HealthCheck(t.Context(), "sb") }()
			select {
			case err := <-next:
				if err == nil {
					t.Fatal("canceled connection remained usable")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled stream retained connection gate")
			}
		})
	}
}

func TestClientDeadlineWhileStreamOwnsConnection(t *testing.T) {
	p, stop := pipeClient(t, local.New())
	defer stop()
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Destroy(t.Context(), sb.ID)
	stream, err := p.StreamExec(t.Context(), sb.ID, sandbox.ExecOptions{Argv: []string{"sh", "-c", "printf output"}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	waiting := make(chan error, 1)
	go func() { waiting <- p.HealthCheck(ctx, sb.ID) }()
	<-ctx.Done()
	requireCanceled(t, waiting, context.DeadlineExceeded)
	var output []byte
	for {
		chunk, err := stream.Recv()
		output = append(output, chunk...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("active stream interrupted: %v", err)
		}
	}
	if string(output) != "output" {
		t.Fatalf("stream output = %q", output)
	}
	// EOF releases the gate and detaches the cancellation callback even before Close.
	if err := p.HealthCheck(t.Context(), sb.ID); err != nil {
		t.Fatal(err)
	}
}
