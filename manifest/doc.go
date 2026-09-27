// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package manifest is the one resolver of spec 003: Resolve decodes a
// file of topos.latere.ai/v1 documents, checks every field strictly,
// writes the fixed defaults out, validates, refuses a secret value, and
// pins the references between the documents and to the objects a
// Lookup holds, so the topos command and the server resolve a manifest
// to the same bytes. AgentConfig turns a resolved Agent into the pieces
// of a harness.Config that need no I/O, and Bundle keeps a resolved
// Agent with the agents it pins for a session.
//
// The package reads no network: it imports manifest/v1, the harness,
// models and session types it feeds, the YAML decoder and the standard
// library, and reads files only through the fs.FS its caller hands it.
package manifest
