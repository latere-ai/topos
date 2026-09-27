// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

//go:generate go test -run TestOpenAPIIsGenerated -update .

// OpenAPI renders the API's OpenAPI document from the route and error
// tables, with server as its one server URL. api/openapi.yaml is this
// document for the server "/v1", and a test holds the two equal.
func OpenAPI(server string) ([]byte, error) {
	statuses := map[int][]string{}
	for code, row := range codes {
		statuses[row.status] = append(statuses[row.status], code)
	}
	var errorCodes []string
	for code := range codes {
		errorCodes = append(errorCodes, code)
	}
	slices.Sort(errorCodes)

	paths := yaml.MapSlice{}
	byPath := map[string]yaml.MapSlice{}
	var order []string
	for _, rt := range table() {
		if _, ok := byPath[rt.path]; !ok {
			order = append(order, rt.path)
		}
		byPath[rt.path] = append(byPath[rt.path], yaml.MapItem{Key: strings.ToLower(rt.method), Value: operation(rt)})
	}
	for _, p := range order {
		paths = append(paths, yaml.MapItem{Key: p, Value: byPath[p]})
	}
	doc := yaml.MapSlice{
		{Key: "openapi", Value: "3.1.0"},
		{Key: "info", Value: yaml.MapSlice{
			{Key: "title", Value: "Topos API"},
			{Key: "version", Value: "v1"},
			{Key: "description", Value: "Agents, their versions, and the sessions people have with them. Every route is verified and asked of the installation's authorizer; every error is one envelope with a code of the table under components.schemas.Error."},
		}},
		{Key: "servers", Value: []yaml.MapSlice{{{Key: "url", Value: server}}}},
		{Key: "security", Value: []yaml.MapSlice{{{Key: "bearer", Value: []string{}}}}},
		{Key: "paths", Value: paths},
		{Key: "components", Value: yaml.MapSlice{
			{Key: "securitySchemes", Value: yaml.MapSlice{
				{Key: "bearer", Value: yaml.MapSlice{{Key: "type", Value: "http"}, {Key: "scheme", Value: "bearer"}, {Key: "bearerFormat", Value: "JWT"}}},
			}},
			{Key: "schemas", Value: yaml.MapSlice{
				{Key: "Error", Value: yaml.MapSlice{
					{Key: "type", Value: "object"},
					{Key: "required", Value: []string{"error"}},
					{Key: "properties", Value: yaml.MapSlice{{Key: "error", Value: yaml.MapSlice{
						{Key: "type", Value: "object"},
						{Key: "required", Value: []string{"code", "message"}},
						{Key: "properties", Value: yaml.MapSlice{
							{Key: "code", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: errorCodes}}},
							{Key: "message", Value: yaml.MapSlice{{Key: "type", Value: "string"}}},
							{Key: "details", Value: yaml.MapSlice{{Key: "type", Value: "object"}}},
						}},
					}}}},
				}},
			}},
			{Key: "responses", Value: errorResponses(statuses)},
		}},
	}
	return yaml.MarshalWithOptions(doc, yaml.Indent(2), yaml.IndentSequence(true))
}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// operation is one route as the document describes it. x-topos-actions
// names the questions the route asks the authorizer.
func operation(rt route) yaml.MapSlice {
	op := yaml.MapSlice{{Key: "operationId", Value: rt.op}, {Key: "summary", Value: rt.summary}}
	if len(rt.actions) > 0 {
		op = append(op, yaml.MapItem{Key: "x-topos-actions", Value: rt.actions})
	} else {
		op = append(op, yaml.MapItem{Key: "security", Value: []yaml.MapSlice{}})
	}
	var params []yaml.MapSlice
	for _, m := range pathParam.FindAllStringSubmatch(rt.path, -1) {
		params = append(params, yaml.MapSlice{
			{Key: "name", Value: m[1]}, {Key: "in", Value: "path"}, {Key: "required", Value: true},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}}},
		})
	}
	if len(params) > 0 {
		op = append(op, yaml.MapItem{Key: "parameters", Value: params})
	}
	if rt.body > 0 {
		media := yaml.MapSlice{{Key: "application/json", Value: yaml.MapSlice{}}}
		if rt.op == "applyAgent" {
			media = append(media, yaml.MapItem{Key: "application/yaml", Value: yaml.MapSlice{}})
		}
		op = append(op, yaml.MapItem{Key: "requestBody", Value: yaml.MapSlice{{Key: "content", Value: media}}})
	}
	responses := yaml.MapSlice{{Key: strconv.Itoa(rt.status), Value: yaml.MapSlice{{Key: "description", Value: http.StatusText(rt.status)}}}}
	responses = append(responses, yaml.MapItem{Key: "default", Value: yaml.MapSlice{{Key: "$ref", Value: "#/components/responses/Error"}}})
	return append(op, yaml.MapItem{Key: "responses", Value: responses})
}

// errorResponses is the one error response every route may answer, with
// the codes each status carries.
func errorResponses(statuses map[int][]string) yaml.MapSlice {
	var ss []int
	for s := range statuses {
		ss = append(ss, s)
	}
	slices.Sort(ss)
	var lines []string
	for _, s := range ss {
		cs := statuses[s]
		slices.Sort(cs)
		lines = append(lines, strconv.Itoa(s)+": "+strings.Join(cs, ", "))
	}
	return yaml.MapSlice{{Key: "Error", Value: yaml.MapSlice{
		{Key: "description", Value: "An error of the table. By status: " + strings.Join(lines, "; ") + "."},
		{Key: "content", Value: yaml.MapSlice{{Key: "application/json", Value: yaml.MapSlice{
			{Key: "schema", Value: yaml.MapSlice{{Key: "$ref", Value: "#/components/schemas/Error"}}},
		}}}},
	}}}
}

// openAPI is GET /openapi.yaml, with this installation's API root as
// its server, so every path of the document joined to it is the address
// a client outside reaches.
func (c *call) openAPI() error {
	b, err := OpenAPI(c.s.rootURL)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/yaml")
	c.w.WriteHeader(http.StatusOK)
	_, err = c.w.Write(b)
	return err
}
