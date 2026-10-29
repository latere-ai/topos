// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"errors"
	"fmt"
)

// ErrChanged is a write or a forget the files store refused because the
// file is not the version the machine last read (spec 093): another
// session changed it between the read and the write, and nothing was
// overwritten. The call's result is the current-content rule's changed
// text.
var ErrChanged = errors.New("memory: the file changed since it was read")

// Refusal is a request the files store refused with its error envelope
// (spec 093): Message is the store's sentence, and Reason and Detail the
// authorizer's reason and its words, when the envelope's detail names one
// as "<reason>: <words>". The call's result is the sentence and the
// words, which the model reads; its meta carries the reason, or the code
// when no reason was given, as refusal.
type Refusal struct {
	Op      string
	Status  int
	Code    string
	Message string
	Reason  string
	Detail  string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("memory: the files store refused the %s with %d %s: %s", r.Op, r.Status, r.Code, r.Message)
}

// Name is the code a client reads in the call's meta: the authorizer's
// reason when the store named one, the store's code otherwise.
func (r *Refusal) Name() string {
	if r.Reason != "" {
		return r.Reason
	}
	return r.Code
}

// Unreachable is a request the files store did not answer (spec 093): a
// failure of the connection, a request past RequestTimeout, a 5xx, or an
// answer that does not decode. The call's result reads "Memory could not
// be reached: <detail>."
type Unreachable struct {
	Detail string
	Err    error
}

func (u *Unreachable) Error() string {
	if u.Err != nil {
		return "memory: the files store could not be reached: " + u.Detail + ": " + u.Err.Error()
	}
	return "memory: the files store could not be reached: " + u.Detail
}

func (u *Unreachable) Unwrap() error { return u.Err }

// ReadOnly is a change to a folder the session reads and does not change
// (spec 093), refused before any request.
type ReadOnly struct {
	Folder string
}

func (r *ReadOnly) Error() string {
	return "memory: /memory/" + r.Folder + "/ is read only in this session"
}

// TooLarge is a file past MaxFileBytes, refused before it is sent or
// read.
type TooLarge struct {
	Path string
	Size int64
}

func (t *TooLarge) Error() string {
	return fmt.Sprintf("memory: %s is %d bytes; a memory file holds at most %d", t.Path, t.Size, MaxFileBytes)
}
