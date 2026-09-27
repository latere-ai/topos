// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"net/url"
	"slices"
	"strings"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
)

const webFetchSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "pattern": "^[Hh][Tt][Tt][Pp][Ss]?://", "description": "The http or https URL to fetch."}
  },
  "required": ["url"],
  "additionalProperties": false
}`

func webFetchTool() Tool {
	return newBuiltin(NameWebFetch, prompts.Text(prompts.ToolWebFetch), webFetchSchema, Properties{Parallel: true, Effect: EffectExternal}, runWebFetch)
}

type webFetchInput struct {
	URL string `json:"url"`
}

func runWebFetch(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in webFetchInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.FetchNotURL, prompts.Data{"URL": in.URL}), nil)
	}
	f, ok := c.Machine.(machine.Fetcher)
	if !ok {
		return b.result(ctx, c, OutcomeError, prompts.Text(prompts.FetchUnavailable), nil)
	}
	res, err := f.Fetch(ctx, machine.FetchRequest{URL: u.String()})
	switch {
	case err != nil && ctx.Err() != nil:
		return b.result(ctx, c, OutcomeCanceled, prompts.Render(prompts.FetchCanceled, prompts.Data{"URL": u.String()}), nil)
	case harnessError(err):
		return Result{}, err
	case errors.Is(err, context.DeadlineExceeded):
		return b.result(ctx, c, OutcomeTimeout, prompts.Render(prompts.FetchTimeout, prompts.Data{"URL": u.String(), "Timeout": machine.FetchTimeout.String()}), nil)
	case err != nil:
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.FetchFailed, prompts.Data{"URL": u.String(), "Error": strings.TrimPrefix(err.Error(), "machine: ")}), nil)
	}
	media, _, merr := mime.ParseMediaType(res.ContentType)
	if merr != nil {
		media = ""
	}
	var body string
	switch {
	case media == "text/html" || media == "application/xhtml+xml":
		body = HTMLText(res.Body)
	case isText(media, res.Body):
		body = strings.ToValidUTF8(string(res.Body), "\uFFFD")
	default:
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.FetchNotText, prompts.Data{"URL": res.URL, "ContentType": res.ContentType}), nil)
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	page := prompts.Render(prompts.FetchPage, prompts.Data{
		"URL": res.URL, "Status": res.Status, "ContentType": res.ContentType, "Body": body,
		"Truncated": res.Truncated, "Max": machine.FetchMaxBody >> 20,
	})
	outcome := OutcomeOK
	if res.Status >= 400 {
		outcome = OutcomeError
	}
	return b.result(ctx, c, outcome, page, nil)
}

// textTypes are the non-text/* media types web_fetch returns as text.
var textTypes = []string{
	"application/json", "application/xml", "application/javascript", "application/ecmascript",
	"application/x-javascript", "application/yaml", "application/x-yaml", "application/toml",
	"application/x-sh", "application/sql", "application/graphql", "image/svg+xml",
}

// isText reports whether a body is text: a text/* or known textual type,
// a +json or +xml suffix, or, without a type, a body with no NUL.
func isText(media string, body []byte) bool {
	switch {
	case strings.HasPrefix(media, "text/"), strings.HasSuffix(media, "+json"), strings.HasSuffix(media, "+xml"):
		return true
	case media == "":
		return bytes.IndexByte(body[:min(len(body), binarySniff)], 0) < 0
	}
	return slices.Contains(textTypes, media)
}
