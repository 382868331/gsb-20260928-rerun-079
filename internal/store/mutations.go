package store

import "sort"

// Mutate runs fn with exclusive access and persists every collection
// fn reports as changed. All engine transitions go through here.
func (s *Store) Mutate(fn func(tx *Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &Tx{s: s, dirty: map[string]bool{}}
	if err := fn(tx); err != nil {
		return err
	}
	for kind := range tx.dirty {
		if err := s.persistLocked(kind); err != nil {
			return err
		}
	}
	return nil
}

type Tx struct {
	s     *Store
	dirty map[string]bool
}

func (tx *Tx) mark(kind string) { tx.dirty[kind] = true }

func (tx *Tx) Versions() []Version     { return tx.s.versions }
func (tx *Tx) Instances() []Instance   { return tx.s.instances }
func (tx *Tx) Faults() []InstanceFault { return tx.s.faults }
func (tx *Tx) Releases() []Release     { return tx.s.releases }
func (tx *Tx) Batches() []Batch        { return tx.s.batches }
func (tx *Tx) Events() []Event         { return tx.s.events }

func (tx *Tx) AddVersion(v Version) {
	tx.s.versions = append(tx.s.versions, v)
	sort.SliceStable(tx.s.versions, func(i, j int) bool {
		return tx.s.versions[i].CreatedAtMS < tx.s.versions[j].CreatedAtMS
	})
	tx.mark("versions")
}

func (tx *Tx) UpsertFault(f InstanceFault) {
	for i := range tx.s.faults {
		if tx.s.faults[i].InstanceID == f.InstanceID && tx.s.faults[i].VersionID == f.VersionID {
			if f.FailUpdate {
				tx.s.faults[i].FailUpdate = true
			}
			if f.Unhealthy {
				tx.s.faults[i].Unhealthy = true
			}
			if f.ProbeDelay > 0 {
				tx.s.faults[i].ProbeDelay = f.ProbeDelay
			}
			tx.mark("faults")
			return
		}
	}
	tx.s.faults = append(tx.s.faults, f)
	tx.mark("faults")
}

func (tx *Tx) ClearFaults() {
	tx.s.faults = []InstanceFault{}
	tx.mark("faults")
}

func (tx *Tx) ReplaceInstances(list []Instance) {
	tx.s.instances = list
	tx.mark("instances")
}

func (tx *Tx) SaveInstance(inst Instance) {
	for i := range tx.s.instances {
		if tx.s.instances[i].ID == inst.ID {
			tx.s.instances[i] = inst
			tx.mark("instances")
			return
		}
	}
	tx.s.instances = append(tx.s.instances, inst)
	tx.mark("instances")
}

func (tx *Tx) Instance(id string) *Instance {
	for i := range tx.s.instances {
		if tx.s.instances[i].ID == id {
			return &tx.s.instances[i]
		}
	}
	return nil
}

func (tx *Tx) AddRelease(r Release) {
	tx.s.releases = append(tx.s.releases, r)
	tx.mark("releases")
}

func (tx *Tx) SaveRelease(r Release) {
	for i := range tx.s.releases {
		if tx.s.releases[i].ID == r.ID {
			tx.s.releases[i] = r
			tx.mark("releases")
			return
		}
	}
}

func (tx *Tx) Release(id string) *Release {
	for i := range tx.s.releases {
		if tx.s.releases[i].ID == id {
			return &tx.s.releases[i]
		}
	}
	return nil
}

func (tx *Tx) AddBatch(b Batch) {
	tx.s.batches = append(tx.s.batches, b)
	tx.mark("batches")
}

func (tx *Tx) SaveBatch(b Batch) {
	for i := range tx.s.batches {
		if tx.s.batches[i].ID == b.ID {
			tx.s.batches[i] = b
			tx.mark("batches")
			return
		}
	}
}

func (tx *Tx) Batch(id string) *Batch {
	for i := range tx.s.batches {
		if tx.s.batches[i].ID == id {
			return &tx.s.batches[i]
		}
	}
	return nil
}

func (tx *Tx) AddEvent(e Event) {
	e.Seq = len(tx.s.events) + 1
	tx.s.events = append(tx.s.events, e)
	tx.mark("events")
}

// ResetAll clears the lab (scenario setup / tests).
func (tx *Tx) ResetAll() {
	tx.s.versions = []Version{}
	tx.s.instances = []Instance{}
	tx.s.faults = []InstanceFault{}
	tx.s.releases = []Release{}
	tx.s.batches = []Batch{}
	tx.s.events = []Event{}
	for _, k := range []string{"versions", "instances", "faults", "releases", "batches", "events"} {
		tx.mark(k)
	}
}
