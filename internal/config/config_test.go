// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
		t.Fatalf("got %+v, want the defaults", c)
	}
}

func TestLoadReadsTheVariables(t *testing.T) {
	c, err := Load(env(map[string]string{
		"TOPOS_PUBLIC_ADDR":   "127.0.0.1:9080",
		"TOPOS_INTERNAL_ADDR": "127.0.0.1:9081",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != "127.0.0.1:9080" || c.InternalAddr != "127.0.0.1:9081" {
		t.Fatalf("got %+v", c)
	}
}

func TestBlankIsTheDefault(t *testing.T) {
	c, err := Load(env(map[string]string{"TOPOS_PUBLIC_ADDR": "  "}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr {
		t.Fatalf("PublicAddr = %q, want the default", c.PublicAddr)
	}
}

func TestEveryProblemIsReportedAtOnceAndSorted(t *testing.T) {
	_, err := Load(env(map[string]string{
		"TOPOS_PUBLIC_ADDR":   "nope",
		"TOPOS_INTERNAL_ADDR": "also-nope",
	}))
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	i, j := strings.Index(msg, "TOPOS_INTERNAL_ADDR"), strings.Index(msg, "TOPOS_PUBLIC_ADDR")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("message %q does not name both variables in sorted order", msg)
	}
	if !strings.HasPrefix(msg, "configuration: ") {
		t.Fatalf("message %q lacks the prefix", msg)
	}
}

func TestTheTwoListenersMustDiffer(t *testing.T) {
	_, err := Load(env(map[string]string{
		"TOPOS_PUBLIC_ADDR":   ":9000",
		"TOPOS_INTERNAL_ADDR": ":9000",
	}))
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want the two listeners refused", err)
	}
}

func TestPortZeroTwiceIsTwoSockets(t *testing.T) {
	if _, err := Load(env(map[string]string{
		"TOPOS_PUBLIC_ADDR":   "127.0.0.1:0",
		"TOPOS_INTERNAL_ADDR": "127.0.0.1:0",
	})); err != nil {
		t.Fatalf("port 0 twice refused: %v", err)
	}
}
