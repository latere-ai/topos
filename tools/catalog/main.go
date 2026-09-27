// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command catalog writes models/catalog.json, the model figures built
// into Topos (spec 007). Prices and names come from a Lux model catalog
// directory, one Model manifest per file; input windows and output
// limits come from OpenRouter's model list, saved from its public
// /api/v1/models endpoint, matched by name. OpenRouter's free models
// that take tools are added at price zero, for development runs.
//
//	go run ./tools/catalog -lux ../lux/deploy/catalog/models -openrouter models.json -date 2026-09-27
package main

import (
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
		Modalities struct {
			Input []string `yaml:"input"`
		} `yaml:"modalities"`
	} `yaml:"spec"`
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
	orPath := fs.String("openrouter", "", "OpenRouter's /api/v1/models answer, saved to a file")
	date := fs.String("date", "", "the date the sources were read, YYYY-MM-DD")
	out := fs.String("out", "models/catalog.json", "the catalog to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := run(*luxDir, *orPath, *date, *out); err != nil {
		// The exit code carries the failure when stderr cannot.
		_, _ = fmt.Fprintln(stderr, "catalog:", err)
		return 1
	}
	return 0
}

func run(luxDir, orPath, date, out string) error {
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
	for _, f := range files {
		e, ok, err := fromLux(f, windows)
		if err != nil {
			return err
		}
		if ok {
			c.Models = append(c.Models, e)
		}
	}
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

// normalize makes a Lux name and an OpenRouter id comparable: Lux writes
// a version with hyphens where OpenRouter writes dots.
func normalize(id string) string { return strings.ReplaceAll(strings.ToLower(id), ".", "-") }

func fromLux(path string, windows map[string]openRouterModel) (models.Entry, bool, error) {
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
	switch t.Provider {
	case "anthropic":
		e.Family, e.Dialect = models.FamilyAnthropic, ir.DialectAnthropicMessages
	case "openai":
		e.Family, e.Dialect = models.FamilyOpenAI, ir.DialectOpenAIResponses
	case "gemini":
		return models.Entry{}, false, nil
	default:
		e.Family, e.Dialect = models.FamilyOther, ir.DialectOpenAIChat
	}
	e.Supports.Images = slices.Contains(m.Spec.Modalities.Input, "image")
	if p := m.Spec.Pricing; p != nil {
		if p.Per != 1_000_000 {
			return models.Entry{}, false, fmt.Errorf("%s: prices per %d tokens, not per million", path, p.Per)
		}
		e.Pricing = &models.Pricing{}
		for _, f := range []struct {
			s   string
			dst **models.Price
		}{{p.Input, &e.Pricing.Input}, {p.Output, &e.Pricing.Output}, {p.CachedInput, &e.Pricing.CacheRead}, {p.CacheWrite, &e.Pricing.CacheWrite}} {
			if f.s == "" {
				continue
			}
			v, err := models.ParsePrice(f.s)
			if err != nil {
				return models.Entry{}, false, fmt.Errorf("%s: %w", path, err)
			}
			*f.dst = &v
		}
	}
	for _, id := range []string{t.Provider + "/" + t.Model, t.Model, e.Name} {
		if w, ok := windows[normalize(id)]; ok {
			e.InputWindow, e.MaxOutputTokens = w.ContextLength, w.TopProvider.MaxCompletionTokens
			s := supports(w)
			e.Supports.Thinking, e.Supports.ParallelTools = s.Thinking, s.ParallelTools
			// The hosted Lux names its models as OpenRouter does, with
			// dots in a version, so that spelling resolves too.
			if w.ID != e.Name && !slices.Contains(e.Aliases, w.ID) {
				e.Aliases = append(e.Aliases, w.ID)
			}
			break
		}
	}
	return e, true, nil
}
