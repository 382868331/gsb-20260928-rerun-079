package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store keeps versions, instances and release batches in separate files.
type Store struct {
	mu        sync.Mutex
	dir       string
	services  []Service
	rules     []CompatRule
	versions  []Version
	instances []Instance
	releases  []Release
	batches   []Batch
	ops       []Op
	events    []Event
	eventSeq  int64
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	must := func(name string, dst any) error {
		err := readJSON(filepath.Join(dir, name), dst)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	if err := must("services.json", &s.services); err != nil {
		return nil, err
	}
	if err := must("compat.json", &s.rules); err != nil {
		return nil, err
	}
	if err := must("versions.json", &s.versions); err != nil {
		return nil, err
	}
	if err := must("instances.json", &s.instances); err != nil {
		return nil, err
	}
	if err := must("releases.json", &s.releases); err != nil {
		return nil, err
	}
	if err := must("batches.json", &s.batches); err != nil {
		return nil, err
	}
	if err := must("ops.json", &s.ops); err != nil {
		return nil, err
	}
	if err := must("events.json", &s.events); err != nil {
		return nil, err
	}
	// Never hand out nil slices: they would marshal as JSON null and break
	// clients that iterate the collections (the empty-environment crash).
	if s.services == nil {
		s.services = []Service{}
	}
	if s.rules == nil {
		s.rules = []CompatRule{}
	}
	if s.versions == nil {
		s.versions = []Version{}
	}
	if s.instances == nil {
		s.instances = []Instance{}
	}
	if s.releases == nil {
		s.releases = []Release{}
	}
	if s.batches == nil {
		s.batches = []Batch{}
	}
	if s.ops == nil {
		s.ops = []Op{}
	}
	if s.events == nil {
		s.events = []Event{}
	}
	s.eventSeq = int64(len(s.events))
	return s, nil
}

func readJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}

// Mutate runs fn under the lock and persists every affected collection
// atomically before returning. fn must perform all changes on the slices
// passed through the closure.
func (s *Store) Mutate(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Tx{s: s}
	if err := fn(tx); err != nil {
		return err
	}
	return s.flush()
}

// View runs fn under the lock without writing.
func (s *Store) View(fn func(tx *Tx)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&Tx{s: s})
}

type Tx struct{ s *Store }

func (tx *Tx) Services() []Service       { return tx.s.services }
func (tx *Tx) CompatRules() []CompatRule { return tx.s.rules }
func (tx *Tx) Versions() []Version       { return tx.s.versions }
func (tx *Tx) Instances() []Instance     { return tx.s.instances }
func (tx *Tx) Releases() []Release       { return tx.s.releases }
func (tx *Tx) Batches() []Batch          { return tx.s.batches }
func (tx *Tx) Ops() []Op                 { return tx.s.ops }
func (tx *Tx) Events() []Event           { return tx.s.events }

func (tx *Tx) SetServices(v []Service)       { tx.s.services = v }
func (tx *Tx) SetCompatRules(v []CompatRule) { tx.s.rules = v }
func (tx *Tx) SetVersions(v []Version)       { tx.s.versions = v }
func (tx *Tx) SetInstances(v []Instance)     { tx.s.instances = v }
func (tx *Tx) SetReleases(v []Release)       { tx.s.releases = v }
func (tx *Tx) SetBatches(v []Batch)          { tx.s.batches = v }
func (tx *Tx) SetOps(v []Op)                 { tx.s.ops = v }

func (tx *Tx) AddEvent(at, releaseID, batchID, instanceID, level, msg string) {
	tx.s.eventSeq++
	tx.s.events = append(tx.s.events, Event{
		Seq: tx.s.eventSeq, At: at, ReleaseID: releaseID, BatchID: batchID,
		InstanceID: instanceID, Level: level, Message: msg,
	})
}

func (s *Store) flush() error {
	writes := []struct {
		name string
		val  any
	}{
		{"services.json", s.services},
		{"compat.json", s.rules},
		{"versions.json", s.versions},
		{"instances.json", s.instances},
		{"releases.json", s.releases},
		{"batches.json", s.batches},
		{"ops.json", s.ops},
		{"events.json", s.events},
	}
	for _, w := range writes {
		if err := writeJSONAtomic(filepath.Join(s.dir, w.name), w.val); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONAtomic(path string, val any) error {
	data, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
