// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package billing

import (
	"fmt"

	"latere.ai/x/topos/models"
)

// CostSource prices a turn's usage. Implementations are consulted once per
// turn, after usage is known.
type CostSource interface {
	// CostUSD returns the cost of u for model. It returns an error when the
	// model cannot be priced, which callers treat as fatal rather than free.
	CostUSD(model string, u models.Usage) (float64, error)
}

// Cache-token multiples of a model's input rate. Cached tokens are not billed
// at the input rate: Anthropic prices a cache read at a tenth of it and a cache
// write at a quarter above it. Agentic loops are cache-heavy by construction —
// every turn re-sends the transcript behind a cache breakpoint — so a flat
// input/output table misprices essentially every real run.
//
// The write multiple is the one for the default five-minute cache TTL. A
// one-hour-TTL write bills at twice the input rate; [models.Usage] carries a
// single CacheWriteTokens count with no TTL, so a run that opts into the long
// TTL is priced low by this card.
const (
	cacheReadMultiple  = 0.1
	cacheWriteMultiple = 1.25
)

// Rate is one model's price, in USD per million tokens, plus the multiples that
// scale the input rate for cached tokens.
type Rate struct {
	// InputPerMTok is the price of a million uncached prompt tokens.
	InputPerMTok float64
	// OutputPerMTok is the price of a million completion tokens.
	OutputPerMTok float64
	// CacheReadMultiple scales InputPerMTok for tokens read from the prompt
	// cache.
	CacheReadMultiple float64
	// CacheWriteMultiple scales InputPerMTok for tokens written to the prompt
	// cache.
	CacheWriteMultiple float64
}

// RateCard prices usage from a pinned per-model table, keyed by model id. It is
// the local fallback for turns the gateway did not price. The table is code-
// pinned and updated by edit: a model the card does not cover cannot be priced,
// and a budget configured against it is refused rather than run unenforced.
type RateCard map[string]Rate

// CostUSD prices u against the card's entry for model. An absent entry is an
// error, never a free turn.
func (c RateCard) CostUSD(model string, u models.Usage) (float64, error) {
	r, ok := c[model]
	if !ok {
		return 0, fmt.Errorf("billing: no rate card entry for model %q", model)
	}
	perInputTok := r.InputPerMTok / 1e6
	cost := float64(u.InputTokens) * perInputTok
	cost += float64(u.OutputTokens) * r.OutputPerMTok / 1e6
	cost += float64(u.CacheReadTokens) * perInputTok * r.CacheReadMultiple
	cost += float64(u.CacheWriteTokens) * perInputTok * r.CacheWriteMultiple
	return cost, nil
}

// DefaultRateCard returns the pinned price table, one entry per model the
// runtime targets. Prices are USD per million tokens as published for the
// Claude API.
//
// Models are omitted deliberately rather than estimated. A model whose price is
// promotional (an introductory rate with an end date), access-restricted, or
// simply unverified is left out, so a budget set against it fails closed at
// construction instead of enforcing against a wrong number. Adding a model is
// an edit to this table.
//
// Each call returns a fresh map, so a host may amend its copy without mutating
// the runtime's.
func DefaultRateCard() RateCard {
	base := map[string][2]float64{
		"claude-fable-5":    {10, 50},
		"claude-opus-4-8":   {5, 25},
		"claude-opus-4-7":   {5, 25},
		"claude-opus-4-6":   {5, 25},
		"claude-sonnet-4-6": {3, 15},
		"claude-haiku-4-5":  {1, 5},
	}
	card := make(RateCard, len(base))
	for id, p := range base {
		card[id] = Rate{
			InputPerMTok:       p[0],
			OutputPerMTok:      p[1],
			CacheReadMultiple:  cacheReadMultiple,
			CacheWriteMultiple: cacheWriteMultiple,
		}
	}
	return card
}

// GatewayFirst prefers the gateway-reported cost and consults Fallback when the
// gateway did not report one. The gateway is the authority on price when it
// answers: it knows the deal the call was actually billed under, which a
// code-pinned card only approximates.
type GatewayFirst struct {
	// Fallback prices turns the gateway did not. A nil Fallback makes every
	// unreported turn an error, which is the fail-closed reading.
	Fallback CostSource
}

// CostUSD returns the reported cost when there is one, and otherwise defers to
// Fallback.
//
// A reported cost has three states, not two. Nil means the gateway said
// nothing. A negative value is the gateway's cannot-price sentinel (Lux records
// -1 for a model its own card does not cover), which is a claim of ignorance,
// not a cost of one millionth of a cent — reading it as a number would
// under-count by the full price of the turn and silently defeat the cap. Both
// fall through to Fallback. Only a non-negative value is a cost, and zero is a
// real one: a local or fully cached call bills nothing and says so.
func (g GatewayFirst) CostUSD(model string, u models.Usage) (float64, error) {
	if u.CostUSDMicro != nil && *u.CostUSDMicro >= 0 {
		return float64(*u.CostUSDMicro) / 1e6, nil
	}
	if g.Fallback == nil {
		return 0, fmt.Errorf("billing: model %q reported no cost and no fallback rate card is configured", model)
	}
	return g.Fallback.CostUSD(model, u)
}

// DefaultCostSource returns the runtime's cost source: the gateway-reported
// figure when the model returns one, and the pinned rate card otherwise.
func DefaultCostSource() CostSource {
	return GatewayFirst{Fallback: DefaultRateCard()}
}
