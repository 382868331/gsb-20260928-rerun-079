package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store persists versions, instances, batches, releases and events in
// separate JSON files (the requirement mandates independent storage).
// Writes are atomic (temp file + rename) so a crash mid-write never
// corrupts progress; Refresh after restart shows the real state.
type Store struct {
	mu  sync.RWMutex
	dir string

	versions  []Version
	instances []Instance
	faults    []InstanceFault
	releases  []Release
	batches   []Batch
	events    []Event
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	loads := []struct {
		file string
		dst  any
	}{
		{"versions.json", &s.versions},
		{"instances.json", &s.instances},
		{"faults.json", &s.faults},
		{"releases.json", &s.releases},
		{"batches.json", &s.batches},
		{"events.json", &s.events},
	}
	for _, l := range loads {
		if err := readJSON(filepath.Join(s.dir, l.file), l.dst); err != nil {
			return err
		}
	}
	if s.versions == nil {
		s.versions = []Version{}
	}
	if s.instances == nil {
		s.instances = []Instance{}
	}
	if s.faults == nil {
		s.faults = []InstanceFault{}
	}
	if s.releases == nil {
		s.releases = []Release{}
	}
	if s.batches == nil {
		s.batches = []Batch{}
	}
	if s.events == nil {
		s.events = []Event{}
	}
	return nil
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

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) persistLocked(kind string) error {
	switch kind {
	case "versions":
		return writeJSONAtomic(filepath.Join(s.dir, "versions.json"), s.versions)
	case "instances":
		return writeJSONAtomic(filepath.Join(s.dir, "instances.json"), s.instances)
	case "faults":
		return writeJSONAtomic(filepath.Join(s.dir, "faults.json"), s.faults)
	case "releases":
		return writeJSONAtomic(filepath.Join(s.dir, "releases.json"), s.releases)
	case "batches":
		return writeJSONAtomic(filepath.Join(s.dir, "batches.json"), s.batches)
	case "events":
		return writeJSONAtomic(filepath.Join(s.dir, "events.json"), s.events)
	default:
		return fmt.Errorf("unknown collection %q", kind)
	}
}

func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		Versions:  clone(s.versions),
		Instances: clone(s.instances),
		Faults:    clone(s.faults),
		Releases:  clone(s.releases),
		Batches:   clone(s.batches),
		Events:    clone(s.events),
	}
}

func clone[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}
