package engine

import (
	"fmt"
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

func batchLabel(b *store.Batch) string {
	return fmt.Sprintf("第 %d 批", b.Index+1)
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
