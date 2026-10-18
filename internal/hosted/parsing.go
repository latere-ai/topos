// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/internal/parser"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// AudienceLectio is the audience of the session's token for the parsing
// service read sends a PDF to (spec 080).
const AudienceLectio = "lectio"

// OriginStore is the name a parse's origin gives the files API of a
// session's folder, which the installation's authorizer holds a
// session's parse to: a file of the session's own folder.
const OriginStore = "storage"

// presigner is a files store that presigns a file for a service that
// fetches it itself, as filestore.Arca does.
type presigner interface {
	Presign(ctx context.Context, ref string) (string, error)
}

// parsingReader is the reader read sends a PDF of the session's folder through
// (spec 080), nil when the installation configures no parsing service,
// the session keeps no folder in a files store that presigns, or the
// runner reaches no session credentials: read then answers a PDF as a
// binary file. Each request carries the session's own token for
// AudienceLectio and the workload session, asked of the drive's token
// source at that request; a token that cannot be had fails the read,
// never the turn.
func (b builder) parsingReader(ctx context.Context, s session.Session, files harness.Files) parsing.Reader {
	src := runner.TokensFrom(ctx)
	pre, presigns := files.(presigner)
	if b.o.ParsingURL == "" || s.Folder == nil || !presigns || src == nil {
		return nil
	}
	c, err := parser.New(b.o.ParsingURL, func(ctx context.Context) (string, error) {
		c, err := src.Token(ctx, AudienceLectio, runner.WorkloadSession)
		if err != nil {
			return "", fmt.Errorf("the session's token for %s: %w", AudienceLectio, err)
		}
		return c.Value, nil
	}, otel.HTTPClient())
	if err == nil {
		var r *parser.Reader
		if r, err = parser.NewReader(c, sessionFolder{id: s.ID, store: b.o.Store, files: pre, folder: *s.Folder}, b.pricer(src), b.o.Log); err == nil {
			return r
		}
	}
	// TOPOS_PARSING_URL is checked at start, so this is a defect; read
	// answers a PDF as it does without a service.
	if b.o.Log != nil {
		b.o.Log.ErrorContext(ctx, "the parsing service's client", "session", s.ID, "err", err)
	}
	return nil
}

// sessionFolder is one session's folder as a read through the parsing
// service finds, presigns and names a file of it.
type sessionFolder struct {
	id     string
	store  session.Store
	files  presigner
	folder session.Folder
}

// Locate answers the ref of the file the folder keeps at path with bytes
// of digest sha: an attachment of the session's messages written there,
// recorded with path, the digest and a ref, or the working directory's
// copy of path at that digest (spec 073), read from the session's log.
func (f sessionFolder) Locate(ctx context.Context, path, sha string) (string, error) {
	log, err := f.store.Events(ctx, f.id, 1, 0)
	if err != nil {
		return "", fmt.Errorf("%w: read the session's log: %w", parsing.ErrUnavailable, err)
	}
	want := session.Digest("sha256:" + sha)
	for _, a := range session.Attachments(log) {
		if a.Path == path && a.Blob == want && a.Ref != "" {
			return a.Ref, nil
		}
	}
	if w, ok := session.CopyOf(log).Files[path]; ok && w.SHA256 == sha {
		return session.WorkRef(path), nil
	}
	return "", parsing.ErrNotKept
}

// Presign is the files store's presign of ref.
func (f sessionFolder) Presign(ctx context.Context, ref string) (string, error) {
	return f.files.Presign(ctx, ref)
}

// Origin is ref's place as the authorizer reads it: the folder's owner,
// escaped as one segment, the folder's path, and ref inside it.
func (f sessionFolder) Origin(ref string) parser.Origin {
	return parser.Origin{Store: OriginStore, Path: url.PathEscape(f.folder.Owner) + "/" + f.folder.Path + "/" + ref}
}

// pricer prices the tokens of a model a parsing service's reader read
// with (spec 080): the embedded catalog's figures, overlaid by those the
// installation's Lux door for OpenAI serves for the model, read with the
// session's own model key when the installation mints one and with
// TOPOS_MODELS_KEY otherwise. A model no source prices is
// models.ErrUnpriced.
func (b builder) pricer(src *runner.TokenSource) parser.Pricer {
	return func(ctx context.Context, model string) (models.Entry, error) {
		e := b.cat.Overlay(model)
		if priced(e) {
			return e, nil
		}
		door := b.o.Doors.Door(b.o.ModelsURL, ir.DialectOpenAIChat)
		if b.o.ModelsURL == "" || !models.NamesADoor(door) {
			return e, models.ErrUnpriced
		}
		key := b.o.ModelsKey
		if src != nil {
			c, err := src.Token(ctx, runner.AudienceLux, runner.WorkloadSession)
			switch {
			case err == nil:
				key = c.Value
			case !errors.Is(err, runner.ErrNotMinted):
				return models.Entry{}, fmt.Errorf("the session's model key: %w", err)
			}
		}
		served, _, err := dialect.Listed(ctx, otel.HTTPClient(), models.Connection{BaseURL: door, Model: model, Credential: key, Dialect: ir.DialectOpenAIChat})
		if err != nil {
			return models.Entry{}, err
		}
		if e = b.cat.Overlay(model, served); !priced(e) {
			return e, models.ErrUnpriced
		}
		return e, nil
	}
}

// priced reports figures with an input and an output price.
func priced(e models.Entry) bool {
	return e.Pricing != nil && e.Pricing.Input != nil && e.Pricing.Output != nil
}
