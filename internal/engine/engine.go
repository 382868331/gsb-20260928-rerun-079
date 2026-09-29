// Package engine runs the local virtual rollout simulator.
//
// Guarantees exercised by the drills:
//   - Versions, instances and release batches are stored independently.
//   - A late health result carries a generation token; it is discarded
//     unless the instance is still waiting on that exact generation, so
//     a result can never be attributed to the next version.
//   - Cancelling stops further batch expansion immediately; instances
//     already updated get an explicit home via rollback batches.
//   - Every transition is persisted before the clock moves on, so a page
//     refresh (or server restart) shows real progress and an interrupted
//     rollback resumes where it left off.
package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"releaselab/internal/store"
)

const (
	StatusInstanceReady       = "ready"
	StatusInstanceUpdating    = "updating"
	StatusInstanceObserving   = "observing"
	StatusInstanceUpdateFail  = "update_failed"
	StatusInstanceRollingBack = "rolling_back"

	HealthUnknown = "unknown"
	Healthy       = "healthy"
	Unhealthy     = "unhealthy"

	ReleasePending     = "pending"
	ReleaseRollingOut  = "rolling_out"
	ReleasePaused      = "paused"
	ReleaseFailing     = "failing"
	ReleaseRollingBack = "rolling_back"
	ReleaseSucceeded   = "succeeded"
	ReleaseCancelled   = "cancelled"
	ReleaseFailed      = "failed"

	BatchPending     = "pending"
	BatchUpdating    = "updating"
	BatchObserving   = "observing"
	BatchHealthy     = "healthy"
	BatchFailed      = "failed"
	BatchRollingBack = "rolling_back"
	BatchRolledBack  = "rolled_back"

	PhaseRollout  = "rollout"
	PhaseRollback = "rollback"

	ActionUpdate   = "update"
	ActionRollback = "rollback"
)

type pendingProbe struct {
	token      string
	instanceID string
	dueMS      int64
	healthy    bool
}

type Engine struct {
	st *store.Store

	mu      sync.Mutex
	nowMS   int64
	paused  bool
	probes  map[string]pendingProbe // token -> probe
	tickMS  int64
	stopCh  chan struct{}
	stopped bool
}

func New(st *store.Store) *Engine {
	e := &Engine{
		st:     st,
		nowMS:  time.Now().UnixMilli(),
		probes: map[string]pendingProbe{},
		tickMS: 50,
		stopCh: make(chan struct{}),
	}
	e.rearmProbesFromDisk()
	return e
}

func (e *Engine) Start() {
	go e.loop()
}

func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stopped {
		e.stopped = true
		close(e.stopCh)
	}
}

func (e *Engine) SetPaused(p bool) {
	e.mu.Lock()
	e.paused = p
	e.mu.Unlock()
}

func (e *Engine) Paused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused
}

func (e *Engine) loop() {
	t := time.NewTicker(time.Duration(e.tickMS) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			e.mu.Lock()
			e.nowMS = time.Now().UnixMilli()
			paused := e.paused
			e.mu.Unlock()
			if !paused {
				e.tickAt(e.nowMS)
			}
		}
	}
}

// Advance moves the virtual clock by dms and runs one state-machine
// pass synchronously. For deterministic tests don't call Start().
func (e *Engine) Advance(dms int64) int64 {
	e.mu.Lock()
	e.nowMS += dms
	now := e.nowMS
	e.mu.Unlock()
	e.tickAt(now)
	return now
}

func (e *Engine) tickAt(now int64) {
	e.deliverDueProbes(now)
	_ = e.st.Mutate(func(tx *store.Tx) error {
		r := activeRelease(tx)
		if r == nil || r.Status == ReleasePaused {
			return nil
		}
		// Finish any in-flight update/rollback actions whose virtual
		// duration elapsed, regardless of which phase the release is in.
		e.completeDueActions(tx, r, now)
		switch r.Status {
		case ReleasePending, ReleaseRollingOut:
			e.advanceRollout(tx, r, now)
		case ReleaseFailing:
			e.planAndStartRollback(tx, r, now)
		case ReleaseRollingBack:
			e.advanceRollback(tx, r, now)
		}
		return nil
	})
}

func activeRelease(tx *store.Tx) *store.Release {
	var active *store.Release
	for i := range tx.Releases() {
		r := &tx.Releases()[i]
		switch r.Status {
		case ReleasePending, ReleaseRollingOut, ReleasePaused, ReleaseFailing, ReleaseRollingBack:
			if active == nil || r.CreatedAtMS > active.CreatedAtMS {
				active = r
			}
		}
	}
	return active
}

func token() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Engine) log(tx *store.Tx, now int64, kind, msg, releaseID, batchID, instance string) {
	tx.AddEvent(store.Event{
		AtMS:      now,
		Kind:      kind,
		Message:   msg,
		ReleaseID: releaseID,
		BatchID:   batchID,
		Instance:  instance,
	})
}

func faultFor(tx *store.Tx, instanceID, versionID string) *store.InstanceFault {
	for i := range tx.Faults() {
		f := &tx.Faults()[i]
		if f.InstanceID == instanceID && f.VersionID == versionID {
			return f
		}
	}
	return nil
}

func sortedInstances(tx *store.Tx, ids []string) []*store.Instance {
	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}
	out := []*store.Instance{}
	for i := range tx.Instances() {
		if idSet[tx.Instances()[i].ID] {
			out = append(out, &tx.Instances()[i])
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func intMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
