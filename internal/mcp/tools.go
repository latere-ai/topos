// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// MaxPages is the most pages of tools/list the client reads.
const MaxPages = 16

// Prefix begins the name of every tool of a server: mcp__<name>__<tool>.
const Prefix = "mcp__"

// Tool is one tool a server lists.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListTools lists the server's tools, following nextCursor for at most
// MaxPages pages.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	cursor := ""
	for range MaxPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.call(ctx, "tools/list", params, "", nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("mcp: tools/list answered %s: %w", snippet(res), err)
		}
		out = append(out, page.Tools...)
		if cursor = page.NextCursor; cursor == "" {
			return out, nil
		}
	}
	return out, nil
}

// Result is a tools/call's result.
type Result struct {
	ResultType        string          `json:"resultType"`
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// Content is one item of a result's content.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource"`
}

// CallTool calls the server's tool name with args, the headers header
// adds beside the transport's own.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage, header http.Header) (Result, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, name, header)
	if err != nil {
		return Result{}, err
	}
	var r Result
	if err := json.Unmarshal(res, &r); err != nil {
		return Result{}, fmt.Errorf("mcp: tools/call answered %s: %w", snippet(res), err)
	}
	return r, nil
}

// Server is a connection of kind mcp and its client.
type Server struct {
	Connection session.Connection
	Client     *Client
}

// Tools lists the server's tools as the session's tools, and the names of
// those whose schema had a keyword the harness's validator does not
// cover, which is dropped from it. A tool whose full name is not a tool
// name, or whose x-mcp-header marks break the transport's rules, is left
// out.
func (s Server) Tools(ctx context.Context) ([]tools.Tool, []string, error) {
	listed, err := s.Client.ListTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []tools.Tool
	var reduced []string
	for _, t := range listed {
		name := Prefix + s.Connection.Name + "__" + t.Name
		if !tools.ValidName(name) {
			continue
		}
		params, ok := headerParams(t.InputSchema)
		if !ok {
			continue
		}
		schema, cut, err := tools.ReduceSchema(t.InputSchema)
		if err != nil {
			continue
		}
		if cut {
			reduced = append(reduced, t.Name)
		}
		effect := tools.EffectExternal
		if s.Connection.ReadsOnly(t.Name) {
			effect = tools.EffectRead
		}
		out = append(out, &tool{
			server: s, remote: t.Name, params: params,
			def:   tools.Definition{Name: name, Description: t.Description, InputSchema: schema},
			props: tools.Properties{Parallel: true, Effect: effect, Remote: true},
		})
	}
	return out, reduced, nil
}

// tool is one tool of a server as the session's tool.
type tool struct {
	server Server
	remote string
	params []headerParam
	def    tools.Definition
	props  tools.Properties
}

func (t *tool) Definition() tools.Definition { return t.def }
func (t *tool) Properties() tools.Properties { return t.props }

// Run calls the tool at the server. A connection the service answers is
// not connected fails the call with a result naming it and the reason,
// which asks the person to connect it; a result that asks for input
// fails the call, since the session cannot give a server input.
func (t *tool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	conn := t.server.Connection
	res, err := t.server.Client.CallTool(ctx, t.remote, c.Input, headersFor(t.params, c.Input))
	var nc *NotConnected
	switch {
	case errors.As(err, &nc):
		r := tools.Text(tools.OutcomeError, prompts.Render(prompts.CallNotConnected, prompts.Data{"Title": conn.Title, "Reason": cmpOr(nc.Reason, "not connected")}))
		r.Requested = &session.ConnectionRequested{Name: conn.Name, Title: conn.Title, Kind: conn.Kind, Reason: nc.Reason, Scopes: nc.Scopes}
		return r, nil
	case err != nil:
		return tools.Result{}, err
	case res.ResultType == "input_required":
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.CallInputRequired, prompts.Data{"Title": conn.Title})), nil
	}
	return t.result(ctx, c, res)
}

// result is a tools/call's result as the model reads it: its text, and the
// text of what is not text or an image, capped and spilled as every
// tool's is, then its images.
func (t *tool) result(ctx context.Context, c tools.Call, res Result) (tools.Result, error) {
	var texts []string
	var images []lux.Block
	for _, item := range res.Content {
		switch item.Type {
		case "text":
			texts = append(texts, item.Text)
		case "image":
			images = append(images, lux.Block{Type: ir.BlockImage, Image: &lux.Image{MediaType: item.MimeType, Data: item.Data}})
		case "resource_link":
			texts = append(texts, strings.TrimSpace(item.Name+" "+item.URI))
		case "resource":
			if r := item.Resource; r != nil && r.Text != "" {
				texts = append(texts, r.Text)
			} else if r != nil {
				texts = append(texts, r.URI)
			}
		default:
			texts = append(texts, "["+item.Type+"]")
		}
	}
	if len(texts) == 0 && len(images) == 0 && len(bytes.TrimSpace(res.StructuredContent)) > 0 && !bytes.Equal(bytes.TrimSpace(res.StructuredContent), []byte("null")) {
		texts = append(texts, string(res.StructuredContent))
	}
	outcome := tools.OutcomeOK
	if res.IsError {
		outcome = tools.OutcomeError
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(images) == 0 {
		text = prompts.Text(prompts.CallNoContent)
	}
	capped, spill, err := tools.Cap(ctx, c.Machine, c.ID, text, t.props.OutputLimit)
	if err != nil {
		return tools.Result{}, err
	}
	var content []lux.Block
	if capped != "" {
		content = append(content, lux.Block{Type: ir.BlockText, Text: capped})
	}
	return tools.Result{Outcome: outcome, Content: append(content, images...), Spill: spill}, nil
}

// headerParam is an argument a tool's schema marks with x-mcp-header: the
// header's name and the argument's path of properties.
type headerParam struct {
	header string
	path   []string
}

// headerParams are the arguments a schema marks with x-mcp-header, and
// false when a mark breaks the transport's rules: an empty or non-token
// name, one named twice, one on a property of no primitive type, or one
// not reachable through properties alone.
func headerParams(raw json.RawMessage) ([]headerParam, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, true
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	var out []headerParam
	ok := true
	var walk func(v any, path []string, reachable bool)
	walk = func(v any, path []string, reachable bool) {
		m, isObj := v.(map[string]any)
		if !isObj {
			if arr, isArr := v.([]any); isArr {
				for _, e := range arr {
					walk(e, path, false)
				}
			}
			return
		}
		for k, sub := range m {
			switch k {
			case "x-mcp-header":
				name, isStr := sub.(string)
				typ, _ := m["type"].(string)
				if !reachable || !isStr || !token(name) || !slices.Contains([]string{"string", "integer", "boolean"}, typ) ||
					slices.ContainsFunc(out, func(p headerParam) bool { return strings.EqualFold(p.header, name) }) {
					ok = false
					continue
				}
				out = append(out, headerParam{header: name, path: slices.Clone(path)})
			case "properties":
				if props, isObj := sub.(map[string]any); isObj {
					for name, p := range props {
						walk(p, append(slices.Clone(path), name), reachable)
					}
				}
			default:
				walk(sub, path, false)
			}
		}
	}
	walk(root, nil, true)
	return out, ok
}

// token reports whether s is an HTTP field name's token.
func token(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !tchar(s[i]) {
			return false
		}
	}
	return true
}

// tchar reports whether c may appear in a token: a letter, a digit, or one
// of RFC 9110's symbols.
func tchar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// headersFor are the Mcp-Param headers of a call's arguments: each marked
// argument with a value of a primitive type, encoded as the transport
// says; an absent or null one is omitted.
func headersFor(params []headerParam, args json.RawMessage) http.Header {
	if len(params) == 0 {
		return nil
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if dec.Decode(&root) != nil {
		return nil
	}
	h := http.Header{}
	for _, p := range params {
		var v any = root
		for _, k := range p.path {
			m, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = m[k]
		}
		var s string
		switch x := v.(type) {
		case string:
			s = headerValue(x)
		case json.Number:
			s = x.String()
		case bool:
			s = fmt.Sprint(x)
		default:
			continue
		}
		h.Set("Mcp-Param-"+p.header, s)
	}
	return h
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
