// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema is a compiled JSON Schema over the keywords spec 005 names.
type Schema struct {
	types        []string
	props        map[string]*Schema
	required     []string
	additional   *Schema
	noAdditional bool
	items        *Schema
	enum         [][]byte
	constant     []byte
	hasConst     bool
	minimum      *big.Float
	maximum      *big.Float
	minLength    *int
	maxLength    *int
	minItems     *int
	maxItems     *int
	pattern      *regexp.Regexp
	oneOf, anyOf []*Schema
}

// annotations are keywords that describe and do not constrain.
var annotations = []string{"$schema", "$id", "title", "description", "default", "examples", "format", "deprecated", "readOnly", "writeOnly", "$comment"}

var typeNames = []string{"object", "array", "string", "number", "integer", "boolean", "null"}

// CompileSchema compiles a tool's input schema. An empty schema accepts
// any object. A keyword outside the supported set is refused, so a
// constraint is never silently skipped.
func CompileSchema(raw json.RawMessage) (*Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &Schema{}, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	return compile(v, "")
}

func compile(v any, at string) (*Schema, error) {
	if b, ok := v.(bool); ok {
		if b {
			return &Schema{}, nil
		}
		return &Schema{types: []string{}}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: a schema is an object or a boolean", pointer(at))
	}
	s := &Schema{}
	for _, k := range sortedKeys(m) {
		val := m[k]
		var err error
		switch k {
		case "type":
			err = s.setType(val, at)
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s/properties: not an object", pointer(at))
			}
			s.props = map[string]*Schema{}
			for _, name := range sortedKeys(props) {
				if s.props[name], err = compile(props[name], at+"/properties/"+escape(name)); err != nil {
					return nil, err
				}
			}
		case "required":
			list, ok := val.([]any)
			if !ok {
				return nil, fmt.Errorf("%s/required: not an array", pointer(at))
			}
			for _, r := range list {
				name, ok := r.(string)
				if !ok {
					return nil, fmt.Errorf("%s/required: not an array of strings", pointer(at))
				}
				s.required = append(s.required, name)
			}
		case "additionalProperties":
			if b, ok := val.(bool); ok {
				s.noAdditional = !b
			} else if s.additional, err = compile(val, at+"/additionalProperties"); err != nil {
				return nil, err
			}
		case "items":
			if s.items, err = compile(val, at+"/items"); err != nil {
				return nil, err
			}
		case "enum":
			list, ok := val.([]any)
			if !ok || len(list) == 0 {
				return nil, fmt.Errorf("%s/enum: not a non-empty array", pointer(at))
			}
			for _, e := range list {
				s.enum = append(s.enum, canonical(e))
			}
		case "const":
			s.constant, s.hasConst = canonical(val), true
		case "minimum", "maximum":
			n, ok := val.(json.Number)
			if !ok {
				return nil, fmt.Errorf("%s/%s: not a number", pointer(at), k)
			}
			f, _, perr := big.ParseFloat(n.String(), 10, 128, big.ToNearestEven)
			if perr != nil {
				return nil, fmt.Errorf("%s/%s: %w", pointer(at), k, perr)
			}
			if k == "minimum" {
				s.minimum = f
			} else {
				s.maximum = f
			}
		case "minLength", "maxLength", "minItems", "maxItems":
			n, ok := val.(json.Number)
			i, ierr := strconv.Atoi(string(n))
			if !ok || ierr != nil || i < 0 {
				return nil, fmt.Errorf("%s/%s: not a non-negative integer", pointer(at), k)
			}
			switch k {
			case "minLength":
				s.minLength = &i
			case "maxLength":
				s.maxLength = &i
			case "minItems":
				s.minItems = &i
			default:
				s.maxItems = &i
			}
		case "pattern":
			p, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("%s/pattern: not a string", pointer(at))
			}
			if s.pattern, err = regexp.Compile(p); err != nil {
				return nil, fmt.Errorf("%s/pattern: %w", pointer(at), err)
			}
		case "oneOf", "anyOf":
			list, ok := val.([]any)
			if !ok || len(list) == 0 {
				return nil, fmt.Errorf("%s/%s: not a non-empty array", pointer(at), k)
			}
			var subs []*Schema
			for i, sub := range list {
				c, err := compile(sub, at+"/"+k+"/"+strconv.Itoa(i))
				if err != nil {
					return nil, err
				}
				subs = append(subs, c)
			}
			if k == "oneOf" {
				s.oneOf = subs
			} else {
				s.anyOf = subs
			}
		default:
			if !slices.Contains(annotations, k) {
				return nil, fmt.Errorf("%s: the keyword %q is not supported", pointer(at), k)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Schema) setType(v any, at string) error {
	var names []string
	switch t := v.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, e := range t {
			n, ok := e.(string)
			if !ok {
				return fmt.Errorf("%s/type: not a string or an array of strings", pointer(at))
			}
			names = append(names, n)
		}
	default:
		return fmt.Errorf("%s/type: not a string or an array of strings", pointer(at))
	}
	for _, n := range names {
		if !slices.Contains(typeNames, n) {
			return fmt.Errorf("%s/type: %q is not a JSON Schema type", pointer(at), n)
		}
	}
	s.types = names
	return nil
}

// Validate returns the problems of v, a value decoded with UseNumber, as
// "<JSON pointer>: <problem>" lines.
func (s *Schema) Validate(v any) []string {
	var out []string
	s.validate(v, "", &out)
	return out
}

func (s *Schema) validate(v any, at string, out *[]string) {
	add := func(format string, args ...any) {
		*out = append(*out, pointer(at)+": "+fmt.Sprintf(format, args...))
	}
	if s.types != nil {
		if len(s.types) == 0 {
			add("no value is allowed here")
			return
		}
		if !slices.ContainsFunc(s.types, func(t string) bool { return hasType(v, t) }) {
			add("expected %s, got %s", strings.Join(s.types, " or "), typeOf(v))
			return
		}
	}
	if s.enum != nil {
		c := canonical(v)
		if !slices.ContainsFunc(s.enum, func(e []byte) bool { return bytes.Equal(e, c) }) {
			add("must be one of %s", joinEnum(s.enum))
		}
	}
	if s.hasConst && !bytes.Equal(canonical(v), s.constant) {
		add("must be %s", s.constant)
	}
	switch x := v.(type) {
	case map[string]any:
		for _, r := range s.required {
			if _, ok := x[r]; !ok {
				add("missing required property %q", r)
			}
		}
		for _, k := range sortedKeys(x) {
			child := at + "/" + escape(k)
			if p, ok := s.props[k]; ok {
				p.validate(x[k], child, out)
			} else if s.noAdditional {
				*out = append(*out, pointer(child)+": unexpected property")
			} else if s.additional != nil {
				s.additional.validate(x[k], child, out)
			}
		}
	case []any:
		if s.minItems != nil && len(x) < *s.minItems {
			add("fewer than %d items", *s.minItems)
		}
		if s.maxItems != nil && len(x) > *s.maxItems {
			add("more than %d items", *s.maxItems)
		}
		if s.items != nil {
			for i, e := range x {
				s.items.validate(e, at+"/"+strconv.Itoa(i), out)
			}
		}
	case string:
		n := utf8.RuneCountInString(x)
		if s.minLength != nil && n < *s.minLength {
			add("shorter than %d characters", *s.minLength)
		}
		if s.maxLength != nil && n > *s.maxLength {
			add("longer than %d characters", *s.maxLength)
		}
		if s.pattern != nil && !s.pattern.MatchString(x) {
			add("does not match the pattern %s", s.pattern)
		}
	case json.Number:
		f, _, err := big.ParseFloat(x.String(), 10, 128, big.ToNearestEven)
		if err == nil {
			if s.minimum != nil && f.Cmp(s.minimum) < 0 {
				add("less than the minimum %s", s.minimum.Text('g', -1))
			}
			if s.maximum != nil && f.Cmp(s.maximum) > 0 {
				add("greater than the maximum %s", s.maximum.Text('g', -1))
			}
		}
	}
	if s.anyOf != nil && !slices.ContainsFunc(s.anyOf, func(sub *Schema) bool { return len(sub.Validate(v)) == 0 }) {
		add("matches none of the allowed forms")
	}
	if s.oneOf != nil {
		n := 0
		for _, sub := range s.oneOf {
			if len(sub.Validate(v)) == 0 {
				n++
			}
		}
		if n != 1 {
			add("matches %d of the forms, exactly one is allowed", n)
		}
	}
}

func hasType(v any, t string) bool {
	switch t {
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, _, err := big.ParseFloat(n.String(), 10, 128, big.ToNearestEven)
		return err == nil && f.IsInt()
	case "number":
		_, ok := v.(json.Number)
		return ok
	}
	return typeOf(v) == t
}

func typeOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "unknown"
}

// canonical renders a decoded value as compact JSON with sorted keys.
func canonical(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func joinEnum(enum [][]byte) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
}

// pointer renders a JSON pointer, "/" for the root.
func pointer(at string) string {
	if at == "" {
		return "/"
	}
	return at
}

func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
