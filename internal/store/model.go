package store

// Version is an immutable software artifact record.
type Version struct {
	ID          string `json:"id"`
	Number      string `json:"number"`
	Artifact    string `json:"artifact"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

// Instance is a local virtual instance. CurrentVersion is always the
// authoritative pointer to the version the instance is running now.
type Instance struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	CurrentVersion  string `json:"current_version"`
	DesiredVersion  string `json:"desired_version,omitempty"`
	State           string `json:"state"`
	Health          string `json:"health"`
	ReleaseID       string `json:"release_id,omitempty"`
	BatchID         string `json:"batch_id,omitempty"`
	Action          string `json:"action,omitempty"`
	ActionVersion   string `json:"action_version,omitempty"`
	GenerationToken string `json:"generation_token,omitempty"`
	ActionStartMS   int64  `json:"action_start_ms,omitempty"`
	UpdatedAtMS     int64  `json:"updated_at_ms"`
}

// Instance fault injection for the drill. FailUpdate/Unhealthy are keyed
// by version id so a fault only applies while the target version is active.
type InstanceFault struct {
	InstanceID string `json:"instance_id"`
	VersionID  string `json:"version_id"`
	FailUpdate bool   `json:"fail_update"`
	Unhealthy  bool   `json:"unhealthy"`
	ProbeDelay int    `json:"probe_delay_ms"`
}

// ProbeResult is one health observation tied to a generation token.
// A late result is only accepted when the token still matches the
// instance's current in-flight generation, so it can never be
// attributed to the next version.
type ProbeResult struct {
	Token     string `json:"token"`
	Instance  string `json:"instance"`
	Version   string `json:"version"`
	Healthy   bool   `json:"healthy"`
	ArrivedMS int64  `json:"arrived_ms"`
}

// Batch is one wave of a rollout or rollback.
type Batch struct {
	ID              string   `json:"id"`
	ReleaseID       string   `json:"release_id"`
	Index           int      `json:"index"`
	Phase           string   `json:"phase"`
	Status          string   `json:"status"`
	InstanceIDs     []string `json:"instance_ids"`
	HealthyCount    int      `json:"healthy_count"`
	UnhealthyCount  int      `json:"unhealthy_count"`
	PendingCount    int      `json:"pending_count"`
	UpdateFailedIDs []string `json:"update_failed_ids"`
	StartedAtMS     int64    `json:"started_at_ms,omitempty"`
	DeadlineMS      int64    `json:"deadline_ms,omitempty"`
	FinishedAtMS    int64    `json:"finished_at_ms,omitempty"`
}

// Release is the top-level rollout/rollback aggregate.
type Release struct {
	ID               string `json:"id"`
	TargetVersionID  string `json:"target_version_id"`
	BaseVersionID    string `json:"base_version_id"`
	Strategy         string `json:"strategy"`
	BatchPercent     []int  `json:"batch_percent"`
	Status           string `json:"status"`
	CurrentBatchIdx  int    `json:"current_batch_index"`
	RollbackBatchIdx int    `json:"rollback_batch_index"`
	RollbackPlanned  bool   `json:"rollback_planned"`
	HealthWaitMS     int    `json:"health_wait_ms"`
	ActionDurationMS int    `json:"action_duration_ms"`
	CancelRequested  bool   `json:"cancel_requested"`
	Reason           string `json:"reason,omitempty"`
	CreatedAtMS      int64  `json:"created_at_ms"`
	UpdatedAtMS      int64  `json:"updated_at_ms"`
	FinishedAtMS     int64  `json:"finished_at_ms,omitempty"`
}

// Event is an append-only entry in the operation timeline.
type Event struct {
	Seq       int    `json:"seq"`
	AtMS      int64  `json:"at_ms"`
	ReleaseID string `json:"release_id,omitempty"`
	BatchID   string `json:"batch_id,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
}

// Snapshot is everything the UI needs; collections are stored in
// separate files on disk and assembled for transport.
type Snapshot struct {
	Versions  []Version       `json:"versions"`
	Instances []Instance      `json:"instances"`
	Faults    []InstanceFault `json:"faults"`
	Releases  []Release       `json:"releases"`
	Batches   []Batch         `json:"batches"`
	Events    []Event         `json:"events"`
	NowMS     int64           `json:"now_ms"`
}
