// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package session is schema v1 of a Topos session (spec 004): the Session
// and Event types, every event type and its payload, the status and stop
// reasons, the identifiers, the pure fold from a log to the transcript a
// model sees, and the Store interface every store implements. A session is
// an append-only log of typed events plus a status; every runner, store
// and client reads and writes it through this package, and the fold is
// defined here once.
//
// The package dials nothing (spec 001, invariant 13): it is pure over the
// bytes and the stores it is handed.
package session
