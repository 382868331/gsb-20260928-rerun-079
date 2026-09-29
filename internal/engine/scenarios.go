package engine

import (
	"fmt"

	"releaselab/internal/store"
)

// SetupInput builds a deterministic local-virtual fleet for drills.
type SetupInput struct {
	BaseVersion   string // e.g. "v1.0.0"
	TargetVersion string // e.g. "v2.0.0"
	InstanceCount int
	// Faults keyed by (instance index, target version): fail to update
	// or report unhealthy, optionally with a delayed health probe.
	FailUpdateIdx []int
	UnhealthyIdx  []int
	ProbeDelayMS  map[int]int
}

// ResetAndSeed wipes state and creates versions + instances + faults.
func (e *Engine) ResetAndSeed(in SetupInput) error {
	now := e.clock()
	if in.InstanceCount == 0 {
		in.InstanceCount = 4
	}
	if in.BaseVersion == "" {
		in.BaseVersion = "v1.0.0"
	}
	if in.TargetVersion == "" {
		in.TargetVersion = "v2.0.0"
	}
	return e.st.Mutate(func(tx *store.Tx) error {
		tx.ResetAll()
		baseID := "ver-" + in.BaseVersion
		targetID := "ver-" + in.TargetVersion
		tx.AddVersion(store.Version{ID: baseID, Number: in.BaseVersion, Artifact: "local://" + in.BaseVersion + ".tar", CreatedAtMS: now})
		tx.AddVersion(store.Version{ID: targetID, Number: in.TargetVersion, Artifact: "local://" + in.TargetVersion + ".tar", CreatedAtMS: now + 1})
		for i := 0; i < in.InstanceCount; i++ {
			tx.SaveInstance(store.Instance{
				ID:             fmt.Sprintf("inst-%02d", i+1),
				Name:           fmt.Sprintf("node-%02d", i+1),
				CurrentVersion: baseID,
				State:          StatusInstanceReady,
				Health:         Healthy,
				UpdatedAtMS:    now,
			})
		}
		add := func(idxs []int, mutate func(f *store.InstanceFault)) {
			for _, idx := range idxs {
				if idx < 0 || idx >= in.InstanceCount {
					continue
				}
				f := store.InstanceFault{InstanceID: fmt.Sprintf("inst-%02d", idx+1), VersionID: targetID}
				mutate(&f)
				tx.UpsertFault(f)
			}
		}
		add(in.FailUpdateIdx, func(f *store.InstanceFault) { f.FailUpdate = true })
		add(in.UnhealthyIdx, func(f *store.InstanceFault) { f.Unhealthy = true })
		for idx, d := range in.ProbeDelayMS {
			f := store.InstanceFault{
				InstanceID: fmt.Sprintf("inst-%02d", idx+1),
				VersionID:  targetID,
				ProbeDelay: d,
			}
			tx.UpsertFault(f)
		}
		e.log(tx, now, "scenario_seeded",
			fmt.Sprintf("Seeded %d instances on %s, target %s", in.InstanceCount, in.BaseVersion, in.TargetVersion),
			"", "", "")
		return nil
	})
}

// rearmProbesFromDisk rebuilds in-memory pending health checks after a
// restart: any instance persisted mid-observation gets a fresh virtual
// probe deadline relative to restart time. Late/old generations are
// still validated by token when results land.
func (e *Engine) rearmProbesFromDisk() {
	snap := e.st.Snapshot()
	now := e.nowMS
	if now == 0 {
		return
	}
	rel := map[string]store.Release{}
	for _, r := range snap.Releases {
		rel[r.ID] = r
	}
	for _, in := range snap.Instances {
		if in.State != StatusInstanceObserving || in.GenerationToken == "" {
			continue
		}
		r, ok := rel[in.ReleaseID]
		if !ok {
			continue
		}
		f := (*store.InstanceFault)(nil)
		for i := range snap.Faults {
			if snap.Faults[i].InstanceID == in.ID && snap.Faults[i].VersionID == in.CurrentVersion {
				f = &snap.Faults[i]
			}
		}
		healthy := true
		delay := int64(r.HealthWaitMS) * 60 / 100
		if f != nil {
			if f.Unhealthy {
				healthy = false
			}
			if f.ProbeDelay > 0 {
				delay = int64(f.ProbeDelay)
			}
		}
		e.probes[in.GenerationToken] = pendingProbe{
			token:      in.GenerationToken,
			instanceID: in.ID,
			dueMS:      now + delay,
			healthy:    healthy,
		}
	}
}
