package engine_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"releaselab/internal/engine"
	"releaselab/internal/store"
)

func TestDiskFilesAreSeparate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st)
	e.SetClockForTest(1_700_000_000_000)
	if err := e.ResetAndSeed(engine.SetupInput{InstanceCount: 4, UnhealthyIdx: []int{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateRelease("ver-v2.0.0", []int{50, 50}, 2000, 1000); err != nil {
		t.Fatal(err)
	}
	for range make([]int, 120) {
		e.Advance(50)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, en := range entries {
		got = append(got, filepath.Base(en.Name()))
	}
	sort.Strings(got)
	want := []string{"batches.json", "events.json", "faults.json", "instances.json", "releases.json", "versions.json"}
	if len(got) != len(want) {
		t.Fatalf("disk files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("disk files = %v, want %v", got, want)
		}
	}
}
