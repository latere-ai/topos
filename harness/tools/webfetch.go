// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"slices"
	"strings"

	"latere.ai/x/topos/machine"
)

//go:embed descriptions/web_fetch.md
var webFetchDescription string

const webFetchSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "pattern": "^[Hh][Tt][Tt][Pp][Ss]?://", "description": "The http or https URL to fetch."}
  },
  "required": ["url"],
  "additionalProperties": false
}`

func webFetchTool() Tool {
	return newBuiltin(NameWebFetch, webFetchDescription, webFetchSchema, Properties{Parallel: true, Effect: EffectExternal}, runWebFetch)
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
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("%q is not an http or https URL.", in.URL), nil)
	}
	f, ok := c.Machine.(machine.Fetcher)
	if !ok {
		return b.result(ctx, c, OutcomeError, "Web fetch is not available on this machine.", nil)
	}
	res, err := f.Fetch(ctx, machine.FetchRequest{URL: u.String()})
	switch {
	case err != nil && ctx.Err() != nil:
		return b.result(ctx, c, OutcomeCanceled, fmt.Sprintf("The fetch of %s was canceled.", u), nil)
	case errors.Is(err, machine.ErrReleased):
		return Result{}, err
	case errors.Is(err, context.DeadlineExceeded):
		return b.result(ctx, c, OutcomeTimeout, fmt.Sprintf("The fetch of %s passed its timeout of %s.", u, machine.FetchTimeout), nil)
	case err != nil:
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("The fetch of %s failed: %s.", u, strings.TrimPrefix(err.Error(), "machine: ")), nil)
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
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("%s returned %s, which is not text; web_fetch returns text and HTML pages.", res.URL, cmp.Or(res.ContentType, "no content type")), nil)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "URL: %s\nStatus: %d\n", res.URL, res.Status)
	if res.ContentType != "" {
		fmt.Fprintf(&out, "Content-Type: %s\n", res.ContentType)
	}
	out.WriteString("\n")
	out.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		out.WriteString("\n")
	}
	if res.Truncated {
		fmt.Fprintf(&out, "[the body passed %d MiB and was cut there]\n", machine.FetchMaxBody>>20)
	}
	outcome := OutcomeOK
	if res.Status >= 400 {
		outcome = OutcomeError
	}
	return b.result(ctx, c, outcome, out.String(), nil)
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
