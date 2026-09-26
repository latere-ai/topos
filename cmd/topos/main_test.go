// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionPrintsTheIdentity(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "topos dev (") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestNoCommandIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(nil, &bytes.Buffer{}, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "spec 024") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	if code := run([]string{"-nope"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("exit %d", code)
	}
}
