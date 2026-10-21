// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command catalog writes models/catalog.json, the model figures built
// into Topos (spec 007). Prices and names come from a Lux model catalog
// directory, one Model manifest per file, and from a running Lux's model
// list, the answer of a door's GET /v1/models saved to a file, which
// adds every model it sells that the directory does not name; input
// windows, output limits and what a model supports, images among them,
// come from OpenRouter's model list, saved from its public
// /api/v1/models endpoint, matched by name, and a model OpenRouter does
// not list takes its windows from the Lux list. OpenRouter's free models
// that take tools are added at price zero, for development runs.
//
//	go run ./tools/catalog -lux ../lux/deploy/catalog/models -served lux-models.json -openrouter models.json -date 2026-10-21
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
)

type luxModel struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Targets []struct {
			Provider string `yaml:"provider"`
			Model    string `yaml:"model"`
		} `yaml:"targets"`
		Pricing *struct {
			Per         int    `yaml:"per"`
			Input       string `yaml:"input"`
			Output      string `yaml:"output"`
			CachedInput string `yaml:"cachedInput"`
			CacheWrite  string `yaml:"cacheWrite"`
		} `yaml:"pricing"`
	} `yaml:"spec"`
}

// servedModel is one model of a Lux door's model list: the Anthropic
// door's max_input_tokens and max_tokens, or the OpenAI door's
// context_window and max_output_tokens, and its prices.
type servedModel struct {
	ID              string `json:"id"`
	ContextWindow   int64  `json:"context_window"`
	MaxInputTokens  int64  `json:"max_input_tokens"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	MaxTokens       int64  `json:"max_tokens"`
	Pricing         *struct {
		Currency    string `json:"currency"`
		Per         int    `json:"per"`
		Input       string `json:"input"`
		Output      string `json:"output"`
		CachedInput string `json:"cached_input"`
		CacheWrite  string `json:"cache_write"`
	} `json:"pricing"`
}

type openRouterModel struct {
	ID            string `json:"id"`
	ContextLength int64  `json:"context_length"`
	TopProvider   struct {
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
}

func main() { os.Exit(cli(os.Args[1:], os.Stderr)) }

// cli runs the command over args and returns its exit code: 0 on success,
// 2 on a usage error, 1 on any other failure.
func cli(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("catalog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	luxDir := fs.String("lux", "", "a Lux catalog directory of Model manifests")
	served := fs.String("served", "", "a Lux door's GET /v1/models answer, saved to a file (optional)")
	orPath := fs.String("openrouter", "", "OpenRouter's /api/v1/models answer, saved to a file")
	date := fs.String("date", "", "the date the sources were read, YYYY-MM-DD")
	out := fs.String("out", "models/catalog.json", "the catalog to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := run(*luxDir, *served, *orPath, *date, *out); err != nil {
		// The exit code carries the failure when stderr cannot.
		_, _ = fmt.Fprintln(stderr, "catalog:", err)
		return 1
	}
	return 0
}

func run(luxDir, servedPath, orPath, date, out string) error {
	if luxDir == "" || orPath == "" || date == "" {
		return errors.New("-lux, -openrouter and -date are required")
	}
	windows, free, err := readOpenRouter(orPath)
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(luxDir, "*.yaml"))
	if err != nil {
		return err
	}
	c := models.Catalog{Source: "Lux model catalog prices and OpenRouter model windows, read " + date}
	var served []servedModel
	if servedPath != "" {
		c.Source = "Lux model catalog and served model list prices and OpenRouter model windows, read " + date
		if served, err = readServed(servedPath); err != nil {
			return err
		}
	}
	sold := map[string]servedModel{}
	for _, m := range served {
		sold[normalize(m.ID)] = m
	}
	for _, f := range files {
		e, ok, err := fromLux(f, windows, sold)
		if err != nil {
			return err
		}
		if ok {
			c.Models = append(c.Models, e)
		}
	}
	added, err := fromServed(servedPath, served, windows, c.Models)
	if err != nil {
		return err
	}
	c.Models = append(c.Models, added...)
	c.Models = append(c.Models, free...)
	slices.SortFunc(c.Models, func(a, b models.Entry) int { return strings.Compare(a.Name, b.Name) })
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(b, '\n'), 0o644)
}

// readOpenRouter returns the windows of every model by normalized name,
// and the free models that take tools as entries.
func readOpenRouter(path string) (map[string]openRouterModel, []models.Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var list struct {
		Data []openRouterModel `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	windows := map[string]openRouterModel{}
	var free []models.Entry
	zero := models.Price(0)
	for _, m := range list.Data {
		if strings.HasSuffix(m.ID, ":batch") {
			continue
		}
		windows[normalize(m.ID)] = m
		if strings.HasSuffix(m.ID, ":free") && slices.Contains(m.SupportedParameters, "tools") {
			free = append(free, models.Entry{
				Name: "openrouter/" + m.ID, Aliases: []string{m.ID},
				Family: models.FamilyOther, Dialect: ir.DialectOpenAIChat,
				InputWindow: m.ContextLength, MaxOutputTokens: m.TopProvider.MaxCompletionTokens,
				Pricing:  &models.Pricing{Input: &zero, Output: &zero},
				Supports: supports(m),
			})
		}
	}
	return windows, free, nil
}

func supports(m openRouterModel) models.Supports {
	return models.Supports{
		Thinking:      slices.Contains(m.SupportedParameters, "reasoning"),
		Images:        slices.Contains(m.Architecture.InputModalities, "image"),
		ParallelTools: slices.Contains(m.SupportedParameters, "parallel_tool_calls"),
	}
}

// openRouterVendor is the vendor OpenRouter names a Lux provider's
// models under, where it is not the provider's own name.
var openRouterVendor = map[string]string{"gemini": "google"}

// normalize makes a Lux name and an OpenRouter id comparable: Lux writes
// a version with hyphens where OpenRouter writes dots.
func normalize(id string) string { return strings.ReplaceAll(strings.ToLower(id), ".", "-") }

func fromLux(path string, windows map[string]openRouterModel, sold map[string]servedModel) (models.Entry, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return models.Entry{}, false, err
	}
	var m luxModel
	if err := yaml.Unmarshal(b, &m); err != nil {
		return models.Entry{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Spec.Targets) == 0 {
		return models.Entry{}, false, fmt.Errorf("%s: no target", path)
	}
	t := m.Spec.Targets[0]
	e := models.Entry{Name: m.Metadata.Name}
	if t.Model != "" && t.Model != e.Name {
		e.Aliases = []string{t.Model}
	}
	e.Family, e.Dialect = family(t.Provider)
	if p := m.Spec.Pricing; p != nil {
		if p.Per != 1_000_000 {
			return models.Entry{}, false, fmt.Errorf("%s: prices per %d tokens, not per million", path, p.Per)
		}
		pricing, err := prices(p.Input, p.Output, p.CachedInput, p.CacheWrite)
		if err != nil {
			return models.Entry{}, false, fmt.Errorf("%s: %w", path, err)
		}
		e.Pricing = pricing
	}
	ids := []string{t.Provider + "/" + t.Model, t.Model, e.Name}
	if vendor, ok := openRouterVendor[t.Provider]; ok {
		ids = append([]string{vendor + "/" + t.Model}, ids...)
	}
	if !matchOpenRouter(&e, ids, windows) {
		for _, id := range ids {
			if m, ok := sold[normalize(id)]; ok {
				servedWindows(&e, m)
				break
			}
		}
	}
	return e, true, nil
}

// servedWindows takes e's input window and output limit from a Lux list's
// entry, for a model OpenRouter does not list.
func servedWindows(e *models.Entry, m servedModel) {
	e.InputWindow = cmp.Or(m.MaxInputTokens, m.ContextWindow)
	e.MaxOutputTokens = cmp.Or(m.MaxTokens, m.MaxOutputTokens)
}

// readServed reads a Lux door's model list saved to path.
func readServed(path string) ([]servedModel, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []servedModel `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return list.Data, nil
}

// family is the family and dialect of a provider's models. A provider
// Topos has no family of, Gemini's among them, is spoken to on the OpenAI
// Chat dialect, the one Lux's OpenAI door takes for every model it routes.
func family(provider string) (string, ir.Dialect) {
	if f, d, ok := models.ProviderFamily(provider); ok {
		return f, d
	}
	return models.FamilyOther, ir.DialectOpenAIChat
}

// prices are the prices per million tokens a source quotes, as decimals;
// an empty one is unknown.
func prices(input, output, cacheRead, cacheWrite string) (*models.Pricing, error) {
	p := &models.Pricing{}
	for _, f := range []struct {
		s   string
		dst **models.Price
	}{{input, &p.Input}, {output, &p.Output}, {cacheRead, &p.CacheRead}, {cacheWrite, &p.CacheWrite}} {
		if f.s == "" {
			continue
		}
		v, err := models.ParsePrice(f.s)
		if err != nil {
			return nil, err
		}
		*f.dst = &v
	}
	return p, nil
}

// matchOpenRouter takes e's windows and what it supports from the first
// of ids OpenRouter lists, and reports whether one was. What the model
// takes and gives, its window, its output limit and whether it takes
// images, reasons and calls tools in parallel, is OpenRouter's, the one
// source of what a model is; a Lux catalog is the source of what the
// gateway sells it as.
func matchOpenRouter(e *models.Entry, ids []string, windows map[string]openRouterModel) bool {
	// OpenRouter lists a model still in preview under the name with
	// -preview after it, which only a name without an exact match takes.
	for _, id := range slices.Clone(ids) {
		ids = append(ids, id+"-preview")
	}
	for _, id := range ids {
		if w, ok := windows[normalize(id)]; ok {
			e.InputWindow, e.MaxOutputTokens = w.ContextLength, w.TopProvider.MaxCompletionTokens
			e.Supports = supports(w)
			// The hosted Lux names its models as OpenRouter does, with
			// dots in a version, so that spelling resolves too.
			if w.ID != e.Name && !slices.Contains(e.Aliases, w.ID) {
				e.Aliases = append(e.Aliases, w.ID)
			}
			return true
		}
	}
	return false
}

// fromServed are the models of served, a Lux door's model list read from
// path, that no entry of known names, by its name or an alias in either
// spelling of a version. Each is named as the list names it, priced as it quotes in
// USD per million tokens, and takes its windows from OpenRouter, or from
// the list where OpenRouter does not list it. A model also answers to its
// name without the provider, and an Anthropic model to its version
// written with hyphens, the spelling of Anthropic's own API.
func fromServed(path string, served []servedModel, windows map[string]openRouterModel, known []models.Entry) ([]models.Entry, error) {
	taken := map[string]bool{}
	for _, e := range known {
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			taken[normalize(n)] = true
		}
	}
	var added []models.Entry
	for _, m := range served {
		provider, upstream, ok := strings.Cut(m.ID, "/")
		if !ok || upstream == "" {
			return nil, fmt.Errorf("%s: %q names no provider", path, m.ID)
		}
		if taken[normalize(m.ID)] {
			continue
		}
		e := models.Entry{Name: m.ID}
		e.Family, e.Dialect = family(provider)
		aliases := []string{upstream}
		if e.Family == models.FamilyAnthropic {
			hyphens := strings.ReplaceAll(upstream, ".", "-")
			aliases = append(aliases, provider+"/"+hyphens, hyphens)
		}
		for _, a := range aliases {
			if a != e.Name && !slices.Contains(e.Aliases, a) && !taken[normalize(a)] {
				e.Aliases = append(e.Aliases, a)
			}
		}
		if p := m.Pricing; p != nil && p.Per == 1_000_000 && (p.Currency == "" || p.Currency == "USD") {
			pricing, err := prices(p.Input, p.Output, p.CachedInput, p.CacheWrite)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, m.ID, err)
			}
			e.Pricing = pricing
		}
		if !matchOpenRouter(&e, []string{m.ID, upstream}, windows) {
			servedWindows(&e, m)
		}
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			taken[normalize(n)] = true
		}
		added = append(added, e)
	}
	return added, nil
}
