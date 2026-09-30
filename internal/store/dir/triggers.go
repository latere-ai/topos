// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// The kinds of a trigger's objects, each a directory under objects/:
//
//	objects/trigger/<trg_id>.json                   a trigger: its document and its firing record
//	objects/trigger_firing/<trg_id>/<frg_id>.json   one of its firings
//	objects/trigger_key/<trg_id>/<hex>.json         the session one of its keys names; hex is the SHA-256 of the key
const (
	kindTrigger       = "trigger"
	kindTriggerFiring = "trigger_firing"
	kindTriggerKey    = "trigger_key"
)

// triggerFile is a trigger's file. The document is JSON text, kept as a
// string so the file reads as text.
type triggerFile struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Owner         string           `json:"owner"`
	OrgID         string           `json:"org_id,omitempty"`
	AgentID       string           `json:"agent_id"`
	Version       int              `json:"version"`
	Digest        string           `json:"digest"`
	Doc           string           `json:"doc"`
	Suspended     bool             `json:"suspended,omitempty"`
	NextFireAt    *time.Time       `json:"next_fire_at,omitempty"`
	LastFiredAt   *time.Time       `json:"last_fired_at,omitempty"`
	LastSessionID string           `json:"last_session_id,omitempty"`
	Counts        v1.TriggerCounts `json:"counts"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// firingFile is a firing's file.
type firingFile struct {
	ID         string    `json:"id"`
	TriggerID  string    `json:"trigger_id"`
	Dedupe     string    `json:"dedupe"`
	Origin     string    `json:"origin"`
	Envelope   string    `json:"envelope,omitempty"`
	Key        string    `json:"key"`
	Outcome    string    `json:"outcome,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Open       bool      `json:"open,omitempty"`
	Replica    string    `json:"replica,omitempty"`
	Time       time.Time `json:"time"`
	ReceivedAt time.Time `json:"received_at"`
}

// keyFile is the session one key of a trigger names.
type keyFile struct {
	TriggerID string `json:"trigger_id"`
	Key       string `json:"key"`
	SessionID string `json:"session_id"`
}

func toTriggerFile(t store.Trigger) triggerFile {
	return triggerFile{ID: t.ID, Name: t.Name, Owner: t.Owner, OrgID: t.OrgID, AgentID: t.AgentID, Version: t.Version, Digest: t.Digest,
		Doc: string(t.Doc), Suspended: t.Suspended, NextFireAt: t.NextFireAt, LastFiredAt: t.LastFiredAt, LastSessionID: t.LastSessionID,
		Counts: t.Counts, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func (f triggerFile) trigger() store.Trigger {
	utc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		u := t.UTC()
		return &u
	}
	return store.Trigger{ID: f.ID, Name: f.Name, Owner: f.Owner, OrgID: f.OrgID, AgentID: f.AgentID, Version: f.Version, Digest: f.Digest,
		Doc: []byte(f.Doc), Suspended: f.Suspended, NextFireAt: utc(f.NextFireAt), LastFiredAt: utc(f.LastFiredAt), LastSessionID: f.LastSessionID,
		Counts: f.Counts, CreatedAt: f.CreatedAt.UTC(), UpdatedAt: f.UpdatedAt.UTC()}
}

func (f firingFile) firing() store.Firing {
	return store.Firing{ID: f.ID, TriggerID: f.TriggerID, Dedupe: f.Dedupe, Origin: f.Origin, Envelope: []byte(f.Envelope), Key: f.Key,
		Outcome: f.Outcome, Reason: f.Reason, SessionID: f.SessionID, Open: f.Open, Replica: f.Replica, Time: f.Time.UTC(), ReceivedAt: f.ReceivedAt.UTC()}
}

// triggerFiles writes a trigger book's objects as files under root, the
// objects directory.
type triggerFiles struct{ root string }

func (s triggerFiles) triggerPath(id string) string {
	return filepath.Join(s.root, kindTrigger, id+".json")
}

func (s triggerFiles) firingPath(triggerID, id string) string {
	return filepath.Join(s.root, kindTriggerFiring, triggerID, id+".json")
}

func (s triggerFiles) keyPath(triggerID, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.root, kindTriggerKey, triggerID, hex.EncodeToString(sum[:])+".json")
}

func (s triggerFiles) SaveTrigger(t store.Trigger) error {
	return write(s.triggerPath(t.ID), toTriggerFile(t))
}

func (s triggerFiles) SaveFiring(f store.Firing) error {
	if err := s.child(kindTriggerFiring, f.TriggerID); err != nil {
		return err
	}
	return write(s.firingPath(f.TriggerID, f.ID), firingFile{ID: f.ID, TriggerID: f.TriggerID, Dedupe: f.Dedupe, Origin: f.Origin,
		Envelope: string(f.Envelope), Key: f.Key, Outcome: f.Outcome, Reason: f.Reason, SessionID: f.SessionID, Open: f.Open,
		Replica: f.Replica, Time: f.Time, ReceivedAt: f.ReceivedAt})
}

func (s triggerFiles) SaveKey(triggerID, key, sessionID string) error {
	path := s.keyPath(triggerID, key)
	if sessionID == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("dir: remove a trigger's key: %w", err)
		}
		return syncDir(filepath.Dir(path))
	}
	if err := s.child(kindTriggerKey, triggerID); err != nil {
		return err
	}
	return write(path, keyFile{TriggerID: triggerID, Key: key, SessionID: sessionID})
}

// RemoveTrigger removes the trigger's firings and keys before the
// trigger's own file, so a crash between them leaves a trigger with
// fewer firings, never firings of a trigger that is gone.
func (s triggerFiles) RemoveTrigger(id string) error {
	for _, kind := range []string{kindTriggerFiring, kindTriggerKey} {
		if err := os.RemoveAll(filepath.Join(s.root, kind, id)); err != nil {
			return fmt.Errorf("dir: remove the %s files of %s: %w", kind, id, err)
		}
		if err := syncDir(filepath.Join(s.root, kind)); err != nil {
			return err
		}
	}
	if err := os.Remove(s.triggerPath(id)); err != nil {
		return fmt.Errorf("dir: remove trigger %s: %w", id, err)
	}
	return syncDir(filepath.Join(s.root, kindTrigger))
}

// child creates a trigger's directory under kind and makes its entry
// durable before a file is written into it.
func (s triggerFiles) child(kind, id string) error {
	parent := filepath.Join(s.root, kind)
	dir := filepath.Join(parent, id)
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("dir: create the %s files of %s: %w", kind, id, err)
	}
	return syncDir(parent)
}

// loadTriggers reads every trigger, firing and key file into a book. A
// firing or a key of a trigger that has no file is left out, as a
// trigger's delete leaves none; a well-named file that does not decode,
// or holds another object than its name says, is ErrCorrupt.
func loadTriggers(root string, book *store.TriggerBook) error {
	files := triggerFiles{root: root}
	entries, err := os.ReadDir(filepath.Join(root, kindTrigger))
	if err != nil {
		return fmt.Errorf("dir: list triggers: %w", err)
	}
	var ts []store.Trigger
	held := map[string]bool{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || session.CheckID(session.PrefixTrigger, id) != nil {
			continue
		}
		var f triggerFile
		if err := readJSON(files.triggerPath(id), &f); err != nil {
			return err
		}
		if f.ID != id {
			return fmt.Errorf("%w: %s holds trigger %q", session.ErrCorrupt, e.Name(), f.ID)
		}
		ts = append(ts, f.trigger())
		held[id] = true
	}
	var fs []store.Firing
	err = eachChild(filepath.Join(root, kindTriggerFiring), held, func(triggerID, name, path string) error {
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || session.CheckID(session.PrefixFiring, id) != nil {
			return nil
		}
		var f firingFile
		if err := readJSON(path, &f); err != nil {
			return err
		}
		if f.ID != id || f.TriggerID != triggerID {
			return fmt.Errorf("%w: %s holds firing %q of %q", session.ErrCorrupt, name, f.ID, f.TriggerID)
		}
		fs = append(fs, f.firing())
		return nil
	})
	if err != nil {
		return err
	}
	var ks []store.KeySession
	err = eachChild(filepath.Join(root, kindTriggerKey), held, func(triggerID, name, path string) error {
		if !strings.HasSuffix(name, ".json") || len(name) != 64+len(".json") {
			return nil
		}
		var f keyFile
		if err := readJSON(path, &f); err != nil {
			return err
		}
		if f.TriggerID != triggerID || files.keyPath(triggerID, f.Key) != path {
			return fmt.Errorf("%w: %s holds another trigger's key", session.ErrCorrupt, name)
		}
		ks = append(ks, store.KeySession{TriggerID: f.TriggerID, Key: f.Key, SessionID: f.SessionID})
		return nil
	})
	if err != nil {
		return err
	}
	if err := book.Load(ts, fs, ks); err != nil {
		return fmt.Errorf("%w: %w", session.ErrCorrupt, err)
	}
	return nil
}

// eachChild calls fn with every file in the directory of each trigger
// held under dir.
func eachChild(dir string, held map[string]bool, fn func(triggerID, name, path string) error) error {
	triggers, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("dir: list %s: %w", filepath.Base(dir), err)
	}
	for _, t := range triggers {
		if !t.IsDir() || !held[t.Name()] {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, t.Name()))
		if err != nil {
			return fmt.Errorf("dir: list the files of %s: %w", t.Name(), err)
		}
		for _, f := range files {
			if err := fn(t.Name(), f.Name(), filepath.Join(dir, t.Name(), f.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
