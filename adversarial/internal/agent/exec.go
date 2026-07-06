// Package agent drives subprocess invocations of claude and codex.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxScanLine caps the per-line token size for every stdout scanner in
// this package. claude/codex stream-json tool results occasionally
// exceed bufio.Scanner's default 64 KB line limit; 8 MB covers the
// largest observed lines. All scanner sites share this so the streaming
// path, the buffered claude parser, and StreamJSON cannot drift apart
// (a smaller cap there would reject a line the other two accepted).
const maxScanLine = 8 * 1024 * 1024

// Run is one subprocess invocation.
//
// When OnStdoutLine is non-nil, Exec switches to a streaming pipe:
// stdout is read line-by-line and each line is forwarded to the
// callback as it arrives. The full stdout is still buffered into
// Result.Stdout for back-compat parsers. This is the verbose streaming
// path (OnStdoutLine): it surfaces tool-use / thinking events live
// while the agent CLI runs.
type Run struct {
	Bin          string
	Args         []string
	Cwd          string
	Stdin        []byte
	Env          []string
	Deadline     time.Duration
	OnStdoutLine func([]byte)
}

// Result is the outcome of one Exec.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
	Killed   bool
}

// Exec runs r. Cancellation propagates to the child process group;
// SIGINT first, then SIGKILL after a 2s grace.
func Exec(ctx context.Context, r Run) (Result, error) {
	if r.Bin == "" {
		return Result{}, errors.New("agent.Exec: empty Bin")
	}
	if r.Cwd == "" {
		return Result{}, errors.New("agent.Exec: empty Cwd")
	}
	bin := r.Bin
	if abs, err := exec.LookPath(bin); err == nil {
		bin = abs
	}

	callCtx := ctx
	var cancel context.CancelFunc
	if r.Deadline > 0 {
		callCtx, cancel = context.WithTimeout(ctx, r.Deadline)
		defer cancel()
	}

	cmd := exec.CommandContext(callCtx, bin, r.Args...)
	cmd.Dir = r.Cwd
	cmd.Env = r.Env
	cmd.Stdin = bytes.NewReader(r.Stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, true) // SIGINT
		go func() {
			time.Sleep(2 * time.Second)
			signalProcessGroup(cmd, false) // SIGKILL
		}()
		return nil
	}

	start := time.Now()
	var stdoutBytes []byte
	if r.OnStdoutLine == nil {
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		err := cmd.Run()
		stdoutBytes = stdout.Bytes()
		res := Result{
			Stdout:   stdoutBytes,
			Stderr:   stderr.Bytes(),
			Duration: time.Since(start),
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				res.ExitCode = ee.ExitCode()
			}
			if callCtx.Err() != nil {
				res.Killed = true
			}
			return res, err
		}
		return res, nil
	}

	// Streaming path: pipe stdout, fan each line to the callback while
	// also buffering the full stdout for the buffered parser.
	pipe, perr := cmd.StdoutPipe()
	if perr != nil {
		return Result{Duration: time.Since(start)}, perr
	}
	if err := cmd.Start(); err != nil {
		return Result{Duration: time.Since(start), Stderr: stderr.Bytes()}, err
	}
	var stdoutBuf bytes.Buffer
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)
	for sc.Scan() {
		line := sc.Bytes()
		stdoutBuf.Write(line)
		stdoutBuf.WriteByte('\n')
		// The scanner reuses its underlying buffer; copy before
		// handing the bytes to the callback.
		cp := make([]byte, len(line))
		copy(cp, line)
		r.OnStdoutLine(cp)
	}
	waitErr := cmd.Wait()
	stdoutBytes = stdoutBuf.Bytes()
	res := Result{
		Stdout:   stdoutBytes,
		Stderr:   stderr.Bytes(),
		Duration: time.Since(start),
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
		}
		if callCtx.Err() != nil {
			res.Killed = true
		}
		return res, waitErr
	}
	if scErr := sc.Err(); scErr != nil {
		return res, scErr
	}
	return res, nil
}

// CleanEnv returns a copy of os.Environ() with the named keys removed,
// plus the LC_ALL=C stability override.
func CleanEnv(remove ...string) []string {
	excl := map[string]struct{}{
		"ANTHROPIC_API_KEY": {},
	}
	for _, k := range remove {
		excl[k] = struct{}{}
	}
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		if _, drop := excl[k]; drop {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "LC_ALL=C")
	return out
}

// DecodeJSONLine decodes one JSON object that may carry unescaped
// control characters in string fields. Falls back to a sanitizing pass
// if the standard decoder rejects the input.
func DecodeJSONLine(line []byte, dst any) error {
	if err := json.Unmarshal(line, dst); err == nil {
		return nil
	}
	sanitized := sanitizeControls(line)
	if err := json.Unmarshal(sanitized, dst); err != nil {
		// Last-resort: log a hex preview of the offending region.
		return fmt.Errorf("decode JSON after sanitize: %w (preview: %x)", err, line[:min(len(line), 256)])
	}
	return nil
}

// StreamJSON reads stdout line-by-line and feeds each successfully
// decoded raw object to visit. Empty lines and undecodable lines are
// skipped silently.
func StreamJSON(stdout io.Reader, visit func(json.RawMessage) error) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var raw json.RawMessage
		if err := DecodeJSONLine(line, &raw); err != nil {
			continue
		}
		if err := visit(raw); err != nil {
			return err
		}
	}
	return sc.Err()
}

func sanitizeControls(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString := false
	escape := false
	for _, b := range in {
		switch {
		case escape:
			out = append(out, b)
			escape = false
		case b == '\\' && inString:
			out = append(out, b)
			escape = true
		case b == '"':
			out = append(out, b)
			inString = !inString
		case inString && b < 0x20 && b != '\t' && b != '\n' && b != '\r':
			out = append(out, []byte(fmt.Sprintf("\\u%04x", b))...)
		default:
			out = append(out, b)
		}
	}
	return out
}
