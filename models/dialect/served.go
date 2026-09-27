// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"latere.ai/x/topos/models"
)

// maxModelList bounds how much of a door's model list is read.
const maxModelList = 4 << 20

// pricePer is the token count a Lux model list quotes every price per.
const pricePer = 1_000_000

// listed is one entry of a Lux door's model list, in the members of the
// OpenAI and the Anthropic shapes: the OpenAI door names the window
// context_window and the output limit max_output_tokens, the Anthropic
// door max_input_tokens and max_tokens.
type listed struct {
	ID              string   `json:"id"`
	ContextWindow   int64    `json:"context_window"`
	MaxInputTokens  int64    `json:"max_input_tokens"`
	MaxOutputTokens int64    `json:"max_output_tokens"`
	MaxTokens       int64    `json:"max_tokens"`
	InputModalities []string `json:"input_modalities"`
	Pricing         *struct {
		Currency    string `json:"currency"`
		Per         int64  `json:"per"`
		Input       string `json:"input"`
		Output      string `json:"output"`
		CachedInput string `json:"cached_input"`
		CacheWrite  string `json:"cache_write"`
	} `json:"pricing"`
}

// Served reads the figures a Lux door serves for the connection's model
// from the door's model list, GET <base>/v1/models (spec 007): the input
// window, the output limit, the prices and whether the model takes
// images, as the entry the catalog overlays between its own and the
// agent's. A model the list does not name, a list the base does not
// answer with, and prices quoted in another currency or per another
// count are no figures. The error is a base that did not answer at all,
// or a list whose prices are not decimals.
func Served(ctx context.Context, client *http.Client, conn models.Connection) (models.Entry, error) {
	base := strings.TrimRight(conn.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return models.Entry{}, fmt.Errorf("models: read the model list of %s: %w", base, err)
	}
	authorize(req, conn)
	resp, err := client.Do(req)
	if err != nil {
		return models.Entry{}, fmt.Errorf("models: read the model list of %s: %w", base, err)
	}
	var list struct {
		Data []listed `json:"data"`
	}
	read := resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, maxModelList)).Decode(&list) == nil
	if err := resp.Body.Close(); err != nil {
		return models.Entry{}, fmt.Errorf("models: read the model list of %s: %w", base, err)
	}
	if !read {
		return models.Entry{}, nil
	}
	i := slices.IndexFunc(list.Data, func(l listed) bool { return l.ID == conn.Model })
	if i < 0 {
		return models.Entry{}, nil
	}
	return list.Data[i].entry()
}

// entry is the listed figures as a catalog entry; a figure the list
// leaves out stays zero, which the catalog reads as unknown.
func (l listed) entry() (models.Entry, error) {
	e := models.Entry{
		Name:            l.ID,
		InputWindow:     cmp.Or(l.ContextWindow, l.MaxInputTokens),
		MaxOutputTokens: cmp.Or(l.MaxOutputTokens, l.MaxTokens),
		Supports:        models.Supports{Images: slices.Contains(l.InputModalities, "image")},
	}
	p := l.Pricing
	if p == nil || p.Per != pricePer || (p.Currency != "" && p.Currency != "USD") {
		return e, nil
	}
	e.Pricing = &models.Pricing{}
	for _, f := range []struct {
		dst **models.Price
		src string
	}{
		{&e.Pricing.Input, p.Input}, {&e.Pricing.Output, p.Output},
		{&e.Pricing.CacheRead, p.CachedInput}, {&e.Pricing.CacheWrite, p.CacheWrite},
	} {
		if f.src == "" {
			continue
		}
		v, err := models.ParsePrice(f.src)
		if err != nil {
			return models.Entry{}, fmt.Errorf("models: the model list prices %s: %w", l.ID, err)
		}
		*f.dst = &v
	}
	return e, nil
}
