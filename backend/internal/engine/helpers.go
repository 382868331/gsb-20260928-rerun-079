package engine

import (
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"releasectl/internal/store"
)

var idCounter int64

func newID(tx *store.Tx, prefix string) string {
	n := atomic.AddInt64(&idCounter, 1)
	return fmt.Sprintf("%s-%d", prefix, n)
}

// ResetIDSequence is used by deterministic tests.
func ResetIDSequence() { atomic.StoreInt64(&idCounter, 0) }

func atomicLoad() int64 { return atomic.LoadInt64(&idCounter) }

func atomicBump(n int64) { atomic.StoreInt64(&idCounter, n) }

func nowStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func findRelease(tx *store.Tx, id string) *store.Release {
	for i := range tx.Releases() {
		if tx.Releases()[i].ID == id {
			return &tx.Releases()[i]
		}
	}
	return nil
}

func findBatch(tx *store.Tx, id string) *store.Batch {
	for i := range tx.Batches() {
		if tx.Batches()[i].ID == id {
			return &tx.Batches()[i]
		}
	}
	return nil
}

func findMember(tx *store.Tx, batchID, instanceID string) (*store.Batch, int) {
	b := findBatch(tx, batchID)
	if b == nil {
		return nil, -1
	}
	for i := range b.Members {
		if b.Members[i].InstanceID == instanceID {
			return b, i
		}
	}
	return b, -1
}

func findInstance(tx *store.Tx, id string) *store.Instance {
	for i := range tx.Instances() {
		if tx.Instances()[i].ID == id {
			return &tx.Instances()[i]
		}
	}
	return nil
}

func releaseBatches(tx *store.Tx, releaseID string) []*store.Batch {
	var out []*store.Batch
	for i := range tx.Batches() {
		if tx.Batches()[i].ReleaseID == releaseID {
			out = append(out, &tx.Batches()[i])
		}
	}
	return out
}

func inAnyBatch(batches []*store.Batch, instanceID string) bool {
	for _, b := range batches {
		for _, m := range b.Members {
			if m.InstanceID == instanceID {
				return true
			}
		}
	}
	return false
}

// rebuildPendingBatches replaces every not-yet-started batch of a release
// with smaller batches over the instances that no started batch has touched.
// Finished batches and the currently paused batch are preserved exactly, so a
// reopen followed by "shrink" never re-executes work that already happened.
func rebuildPendingBatches(tx *store.Tx, rel *store.Release, size int, now time.Time) error {
	old := releaseBatches(tx, rel.ID)
	sortBatches(old)
	started := map[string]bool{}
	maxIndex := -1
	var remaining []string
	for _, b := range old {
		if b.Phase != "pending" {
			maxIndex = b.Index
			for _, m := range b.Members {
				started[m.InstanceID] = true
			}
		}
	}
	for _, b := range old {
		if b.Phase != "pending" {
			continue
		}
		for _, m := range b.Members {
			if !started[m.InstanceID] {
				remaining = append(remaining, m.InstanceID)
			}
		}
	}
	kept := tx.Batches()[:0]
	for _, b := range tx.Batches() {
		if b.ReleaseID == rel.ID && b.Phase == "pending" {
			continue
		}
		kept = append(kept, b)
	}
	// Pending batches never have ops, but drop any stale ones defensively.
	ops := tx.Ops()[:0]
	pendingIDs := map[string]bool{}
	for _, b := range old {
		if b.Phase == "pending" {
			pendingIDs[b.ID] = true
		}
	}
	for _, op := range tx.Ops() {
		if !pendingIDs[op.BatchID] {
			ops = append(ops, op)
		}
	}
	tx.SetOps(ops)

	idx := maxIndex + 1
	batchIDs := rel.BatchIDs[:0]
	for _, b := range kept {
		if b.ReleaseID == rel.ID {
			batchIDs = append(batchIDs, b.ID)
		}
	}
	for start := 0; start < len(remaining); start += size {
		end := start + size
		if end > len(remaining) {
			end = len(remaining)
		}
		b := store.Batch{
			ID: newID(tx, "batch"), ReleaseID: rel.ID, Index: idx,
			Phase: "pending", TargetVersion: rel.TargetVersion,
		}
		for _, id := range remaining[start:end] {
			inst := findInstance(tx, id)
			from := rel.Baseline
			if inst != nil {
				from = inst.CurrentVersion
			}
			b.Members = append(b.Members, store.MemberState{
				InstanceID: id, FromVersion: from, ToVersion: rel.TargetVersion,
				UpdateState: "pending", HealthState: "pending",
			})
		}
		kept = append(kept, b)
		batchIDs = append(batchIDs, b.ID)
		idx++
	}
	rel.BatchIDs = append([]string(nil), batchIDs...)
	tx.SetBatches(kept)
	return nil
}

func sortBatches(bs []*store.Batch) {
	sort.Slice(bs, func(i, j int) bool { return bs[i].Index < bs[j].Index })
}

// rejectOnFailureEvidence blocks "continue"/"shrink" when the active batch
// already contains a failed update or an unhealthy observation: such a batch
// can only be rolled back or explicitly ended.
func rejectOnFailureEvidence(tx *store.Tx, rel *store.Release, msg string) error {
	for _, b := range releaseBatches(tx, rel.ID) {
		if b.Phase != "paused" {
			continue
		}
		for _, m := range b.Members {
			if m.UpdateState == "failed" || m.HealthState == "unhealthy" {
				return errors.New(msg)
			}
		}
	}
	return nil
}

func batchLabel(b *store.Batch) string {
	return fmt.Sprintf("第 %d 批", b.Index+1)
}

func healthVerdictText(v string) string {
	if v == "healthy" {
		return "健康"
	}
	return "不健康"
}

// updateShouldFail decides the virtual outcome of an update op.
func updateShouldFail(rel store.Release, b store.Batch, m store.MemberState) bool {
	switch rel.Failure.Mode {
	case "instances":
		for _, id := range rel.Failure.Instances {
			if id == m.InstanceID {
				return true
			}
		}
	}
	return false
}

// healthShouldFail realizes the half-batch failure drill: within the batch
// that contains the first listed failing instance, its first half fails
// health observation. Explicit instance listing also works.
func healthShouldFail(rel store.Release, b store.Batch, m store.MemberState) bool {
	switch rel.Failure.Mode {
	case "half_batch":
		if len(b.Members) == 0 {
			return false
		}
		half := len(b.Members) / 2
		if half < 1 {
			half = 1
		}
		for i := 0; i < half; i++ {
			if b.Members[i].InstanceID == m.InstanceID {
				return true
			}
		}
	case "instances":
		for _, id := range rel.Failure.Instances {
			if id == m.InstanceID {
				return true
			}
		}
	}
	return false
}
