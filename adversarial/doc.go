// Package adversarial is a topos capability: a proposer agent and one or more
// critic agents cross-examine a diff over bounded rounds, with per-fork lineage.
// It is a use of the topos runtime, not a peer of it — multi-agent spawning with
// attenuated authority and a deterministic lineage graph, which is exactly what
// topos provides.
//
// The debate runs as N independent forks. In each fork a critic attacks the diff
// aspect by aspect, the proposer concedes or rebuts each attack, and the round
// loop continues until the fork reaches steady state, exhausts its round budget,
// or trips the shared cost cap. An attack ledger tracks every claim across rounds
// so a [Summary] can report what stayed unresolved.
//
// Two surfaces drive a review:
//
//   - [Review] is the thin convenience for the common single call: review a
//     working-tree diff with a proposer and a critic factory over N forks and get
//     a [Summary] back. It builds an [Engine] from [ReviewOptions] and runs it.
//   - [Engine] is the full-control surface. Callers that need per-fork wiring set
//     its fields directly and call [Engine.Run].
//
// Both write session artifacts to sessions/<id>/ under the caller-provided
// StateDir and invent no default location of their own, so topos stays embeddable
// by any host. StateDir is required; leaving it empty is a caller error.
//
// The engine core is backend-agnostic: callers implement [Proposer] and [Critic]
// (or supply a [CriticFactory]). Ready-made backends live in subpackages — a
// Claude-CLI proposer and critic in claude, a topos-native critic in critic, and
// working-tree diff plus Claude transcript helpers in input.
package adversarial
