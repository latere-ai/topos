// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// Entry is one model's figures: its windows, its prices and what it
// supports. A zero figure is unknown, never zero.
type Entry struct {
	Name            string     `json:"name"`
	Aliases         []string   `json:"aliases,omitempty"`
	Family          string     `json:"family,omitempty"`
	Dialect         ir.Dialect `json:"dialect,omitempty"`
	InputWindow     int64      `json:"input_window,omitempty"`
	MaxOutputTokens int64      `json:"max_output_tokens,omitempty"`
	Pricing         *Pricing   `json:"pricing,omitempty"`
	Supports        Supports   `json:"supports,omitzero"`
}

// Supports says which request features a model takes.
type Supports struct {
	Thinking      bool `json:"thinking,omitempty"`
	Effort        bool `json:"effort,omitempty"`
	Images        bool `json:"images,omitempty"`
	ParallelTools bool `json:"parallel_tools,omitempty"`
}

// Price is USD per million tokens, held as micro-USD per million tokens
// so cost is exact: a price of 1.25 is 1250000.
type Price int64

// ParsePrice reads a decimal price with at most six fraction digits.
func ParsePrice(s string) (Price, error) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" || len(frac) > 6 || strings.HasPrefix(whole, "-") {
		return 0, fmt.Errorf("models: price %q is not a non-negative decimal with at most 6 fraction digits", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("models: price %q: %w", s, err)
	}
	var f int64
	if frac != "" {
		f, err = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("models: price %q: %w", s, err)
		}
	}
	return Price(w*1_000_000 + f), nil
}

// String renders the price as a decimal.
func (p Price) String() string {
	s := strconv.FormatInt(int64(p)/1_000_000, 10)
	if f := int64(p) % 1_000_000; f != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%06d", f), "0")
	}
	return s
}

// MarshalJSON writes the price as a decimal string.
func (p Price) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

// UnmarshalJSON reads a decimal string.
func (p *Price) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("models: a price is a decimal string: %w", err)
	}
	v, err := ParsePrice(s)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// Pricing is a model's prices. A nil price is unpriced.
type Pricing struct {
	Input      *Price `json:"input,omitempty"`
	Output     *Price `json:"output,omitempty"`
	CacheRead  *Price `json:"cache_read,omitempty"`
	CacheWrite *Price `json:"cache_write,omitempty"`
}

// cacheRead is the cache-read price, or the input price when the catalog
// has none, which can only overstate a cost.
func (p Pricing) cacheRead() Price {
	if p.CacheRead != nil {
		return *p.CacheRead
	}
	return *p.Input
}

// cacheWrite is the cache-write price, or 1.25 times the input price when
// the catalog has none: the rate of the one provider that reports cache
// writes, since the others bill none.
func (p Pricing) cacheWrite() Price {
	if p.CacheWrite != nil {
		return *p.CacheWrite
	}
	return *p.Input * 5 / 4
}

// Catalog is a set of entries, looked up by name or alias.
type Catalog struct {
	Source string  `json:"source,omitempty"`
	Models []Entry `json:"models"`
}

//go:embed catalog.json
var embedded []byte

// Embedded is the catalog built into this binary.
func Embedded() (Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(embedded, &c); err != nil {
		return Catalog{}, fmt.Errorf("models: the embedded catalog: %w", err)
	}
	return c, nil
}

// Lookup returns the entry whose name or alias is name.
func (c Catalog) Lookup(name string) (Entry, bool) {
	for _, e := range c.Models {
		if e.Name == name || slices.Contains(e.Aliases, name) {
			return e, true
		}
	}
	return Entry{}, false
}

// Overlay returns the figures of the model a connection names: the
// catalog's entry, overlaid by each later source in turn (the figures a
// Lux connection serves, then the agent's own), each non-zero field
// replacing the one before, whether or not any source gives its windows.
func (c Catalog) Overlay(name string, over ...Entry) Entry {
	e, _ := c.Lookup(name)
	if e.Name == "" {
		e.Name = name
	}
	for _, o := range over {
		e = overlay(e, o)
	}
	return e
}

// Resolve is Overlay for a model a harness runs on: one with no input
// window or output limit from any source is model_unknown.
func (c Catalog) Resolve(name string, over ...Entry) (Entry, error) {
	e := c.Overlay(name, over...)
	if e.InputWindow <= 0 || e.MaxOutputTokens <= 0 {
		return e, &Coded{Code: CodeUnknown, Message: fmt.Sprintf("no source gives the input window and output limit of %q", name)}
	}
	return e, nil
}

func overlay(e, o Entry) Entry {
	if o.Family != "" {
		e.Family = o.Family
	}
	if o.Dialect != "" {
		e.Dialect = o.Dialect
	}
	if o.InputWindow > 0 {
		e.InputWindow = o.InputWindow
	}
	if o.MaxOutputTokens > 0 {
		e.MaxOutputTokens = o.MaxOutputTokens
	}
	if o.Pricing != nil {
		p := Pricing{}
		if e.Pricing != nil {
			p = *e.Pricing
		}
		for _, f := range []struct{ dst, src **Price }{
			{&p.Input, &o.Pricing.Input}, {&p.Output, &o.Pricing.Output},
			{&p.CacheRead, &o.Pricing.CacheRead}, {&p.CacheWrite, &o.Pricing.CacheWrite},
		} {
			if *f.src != nil {
				*f.dst = *f.src
			}
		}
		e.Pricing = &p
	}
	s := &e.Supports
	s.Thinking = s.Thinking || o.Supports.Thinking
	s.Effort = s.Effort || o.Supports.Effort
	s.Images = s.Images || o.Supports.Images
	s.ParallelTools = s.ParallelTools || o.Supports.ParallelTools
	return e
}

// Cost sources of a model.request.
const (
	CostProvider = "provider"
	CostCatalog  = "catalog"
)

// ErrUnpriced is a usage the entry cannot price.
var ErrUnpriced = errors.New("models: the model is unpriced")

// Cost prices usage in micro-USD: the provider's reported cost when it
// gives one, otherwise the catalog's prices, rounded up. A model with an
// input and an output price is priced; its cache prices default as
// cacheRead and cacheWrite say. Output includes
// reasoning tokens, which every dialect reports inside its output count,
// so they are priced at the output rate once. It returns the cost
// source, or ErrUnpriced.
func Cost(u UsageFigures, e Entry) (int64, string, error) {
	if u.ReportedCostUSDMicro != nil {
		return *u.ReportedCostUSDMicro, CostProvider, nil
	}
	p := e.Pricing
	if p == nil || p.Input == nil || p.Output == nil {
		return 0, "", ErrUnpriced
	}
	total := u.Input*int64(*p.Input) + u.Output*int64(*p.Output)
	if u.CacheRead != nil {
		total += *u.CacheRead * int64(p.cacheRead())
	}
	if u.CacheWrite != nil {
		total += *u.CacheWrite * int64(p.cacheWrite())
	}
	return (total + 999_999) / 1_000_000, CostCatalog, nil
}

// UsageFigures are one response's token counts. Input excludes the
// tokens read from and written to the prompt cache, which are counted
// apart; a nil cache figure was not reported.
type UsageFigures struct {
	Input, Output         int64
	CacheRead, CacheWrite *int64
	ReportedCostUSDMicro  *int64
}

// FromLux reads the Lux wire usage a model.request records.
func FromLux(u lux.Usage) UsageFigures {
	return UsageFigures{
		Input: u.InputTokens, Output: u.OutputTokens,
		CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheWriteInputTokens,
		ReportedCostUSDMicro: u.CostUSDMicro,
	}
}

// EstimateInput prices input tokens at the input rate, rounded up: the
// budget's pre-request estimate of spec 007.
func EstimateInput(tokens int64, e Entry) (int64, error) {
	if e.Pricing == nil || e.Pricing.Input == nil {
		return 0, ErrUnpriced
	}
	return (tokens*int64(*e.Pricing.Input) + 999_999) / 1_000_000, nil
}
