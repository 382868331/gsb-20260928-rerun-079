package engine

import "releaselab/internal/store"

// SnapshotAt returns persisted state plus the current virtual time.
func (e *Engine) SnapshotAt(now int64) store.Snapshot {
	snap := e.st.Snapshot()
	snap.NowMS = now
	return snap
}
