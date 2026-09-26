// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package arch holds the tests of the structural invariants of spec 001:
// which packages sit at the module root and what each may import, and
// that nothing a fork would inherit, a document, a manifest, a workflow,
// a default, or a comment, names a particular deployment of Topos or
// anything internal to one. The package has no code of its own; the
// tests read the tree.
package arch
