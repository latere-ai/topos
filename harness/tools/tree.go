// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"

	"latere.ai/x/topos/models"
)

// errTrailing is the error of text after the first JSON value.
var errTrailing = errors.New("there is text after the first JSON value")

// NewTreeDecoder is a decoder of the values ReadTree reads, which accepts
// what a model or a person wrote: repeated names and invalid UTF-8.
func NewTreeDecoder(r io.Reader) *jsontext.Decoder {
	return jsontext.NewDecoder(r, models.FromProvider)
}

// DecodeTree decodes raw, one JSON value that a model wrote, into maps,
// lists, strings, booleans and nil, with every number kept as the digits
// it was written with, a jsontext.Value: an argument's exact digits reach
// the validator and the comparison of two calls, which a float would
// round. Text after the value is an error.
func DecodeTree(raw []byte) (any, error) {
	dec := NewTreeDecoder(bytes.NewReader(raw))
	v, err := readTree(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errTrailing
	}
	return v, nil
}

// ReadTree reads the next JSON value of dec as DecodeTree decodes one, for
// a stream of values.
func ReadTree(dec *jsontext.Decoder) (any, error) { return readTree(dec) }

func readTree(dec *jsontext.Decoder) (any, error) {
	switch dec.PeekKind() {
	case '{':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for dec.PeekKind() != '}' {
			k, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			key := k.String()
			v, err := readTree(dec)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		_, err := dec.ReadToken()
		return m, err
	case '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		list := []any{}
		for dec.PeekKind() != ']' {
			v, err := readTree(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.ReadToken()
		return list, err
	case '0':
		v, err := dec.ReadValue()
		return jsontext.Value(bytes.Clone(v)), err
	}
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case '"':
		return tok.String(), nil
	case 't', 'f':
		return tok.Bool(), nil
	}
	return nil, nil
}
