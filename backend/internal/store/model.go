package store

// Service is a virtual service. Services depend on each other; the
// compatibility constraints between them live in CompatRule records.
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CompatRule constrains how a dependent service must behave while its
// dependency is being updated:
//   - dependent_first: the dependent must finish its own update before the
//     dependency's batches may take effect.
//   - stay_old: the dependent must stay on its baseline (no update) while
//     the dependency is updating.
//   - pause: the dependent must not be actively advancing while the
//     dependency updates (its own release must pause or finish first).
type CompatRule struct {
	ID           string `json:"id"`
	DependentID  string `json:"dependentId"`
	DependencyID string `json:"dependencyId"`
	Mode         string `json:"mode"` // dependent_first | stay_old | pause
	Note         string `json:"note"`
}

// CompatCheck is one persisted compatibility/health verdict evaluated right
// before a batch takes effect. It is the evidence for why a batch was
// allowed to expand or is still waiting.
type CompatCheck struct {
	ServiceID string `json:"serviceId"`
	Kind      string `json:"kind"` // compat | health
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
}

// Version is an immutable artifact record. Versions, instances and release
// batches are stored in separate files on purpose.
type Version struct {
	ID          string `json:"id"`
	ServiceID   string `json:"serviceId"`
	Name        string `json:"name"`
	ArtifactRef string `json:"artifactRef"`
	CreatedAt   string `json:"createdAt"`
}

// Instance is a local virtual instance. DesiredVersion is what the fleet
// converges back to when no release is active (the known-good baseline).
type Instance struct {
	ID             string `json:"id"`
	ServiceID      string `json:"serviceId"`
	Name           string `json:"name"`
	CurrentVersion string `json:"currentVersion"`
	DesiredVersion string `json:"desiredVersion"`
	Health         string `json:"health"` // healthy | unhealthy | unknown
	UpdatedAt      string `json:"updatedAt"`
}

// MemberState tracks a single instance inside a batch.
type MemberState struct {
	InstanceID      string `json:"instanceId"`
	FromVersion     string `json:"fromVersion"`
	ToVersion       string `json:"toVersion"`
	UpdateState     string `json:"updateState"` // pending|in_progress|succeeded|failed|rollback_in_progress|rolled_back
	HealthState     string `json:"healthState"` // pending|in_progress|healthy|unhealthy|stale
	Message         string `json:"message"`
	LastResolvedKey string `json:"lastResolvedKey"`
	// LateHealth* records a health answer that arrived after the member's fate
	// was already decided (review cancel/rollback, or the instance moved on).
	// The verdict stays queryable as evidence but can never change the
	// authoritative HealthState or the release's terminal decision.
	LateHealthResult  string `json:"lateHealthResult,omitempty"` // healthy|unhealthy
	LateHealthVersion string `json:"lateHealthVersion,omitempty"`
	LateHealthAt      string `json:"lateHealthAt,omitempty"`
}

// Batch is stored independently from releases.
type Batch struct {
	ID            string        `json:"id"`
	ReleaseID     string        `json:"releaseId"`
	Index         int           `json:"index"`
	Phase         string        `json:"phase"` // pending|updating|observing|succeeded|failed|cancelling|rolling_back|rolled_back|cancelled
	TargetVersion string        `json:"targetVersion"`
	Members       []MemberState `json:"members"`
	// Compat records the compatibility and health evidence evaluated before
	// this batch takes effect. BlockedReason is set while the batch waits for
	// another service to satisfy a constraint.
	Compat        []CompatCheck `json:"compat,omitempty"`
	BlockedReason string        `json:"blockedReason,omitempty"`
	StartedAt     string        `json:"startedAt,omitempty"`
	FinishedAt    string        `json:"finishedAt,omitempty"`
}

// FailureSpec injects failures for a drill without deploying anything.
type FailureSpec struct {
	Mode               string   `json:"mode"` // none|half_batch|instances|rollback_interrupt
	Instances          []string `json:"instances,omitempty"`
	FailFirstNRollback int      `json:"failFirstNRollback,omitempty"`
	PauseOnFailure     bool     `json:"pauseOnFailure,omitempty"`
}

type Release struct {
	ID              string      `json:"id"`
	ServiceID       string      `json:"serviceId"`
	TargetVersion   string      `json:"targetVersion"`
	Baseline        string      `json:"baselineVersion"`
	Strategy        string      `json:"strategy"` // rolling
	BatchSize       int         `json:"batchSize"`
	BatchIDs        []string    `json:"batchIds"`
	Status          string      `json:"status"` // pending|in_progress|paused|succeeded|failed|cancelling|rolling_back|rolled_back|cancelled
	Failure         FailureSpec `json:"failure"`
	CancelRequested bool        `json:"cancelRequested"`
	ResumeRequested bool        `json:"resumeRequested"`
	FinishRequested bool        `json:"finishRequested"`
	// PauseRequested freezes expansion and auto-decisions while scheduled
	// evidence (health results) is still collected. ReviewAction records the
	// operator's decision so a reopen never replays a finished batch.
	PauseRequested bool   `json:"pauseRequested,omitempty"`
	ReviewAction   string `json:"reviewAction,omitempty"` // ""|continue|shrink|rollback
	ReviewShrinkTo int    `json:"reviewShrinkTo,omitempty"`
	ReviewAt       string `json:"reviewAt,omitempty"`
	// RollbackImpact explains, once the operator chooses rollback, whether
	// dependent services stay compatible and which already-finished batches
	// cannot be undone directly.
	RollbackImpact []string `json:"rollbackImpact,omitempty"`
	CreatedAt      string   `json:"createdAt"`
	FinishedAt     string   `json:"finishedAt,omitempty"`
	Message        string   `json:"message,omitempty"`
}

// Op is a virtual asynchronous operation with a scheduled due time. All
// progress lives on disk so a process restart resumes exactly where it left
// off (including mid-rollback).
type Op struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // update|health|rollback
	ReleaseID   string `json:"releaseId"`
	BatchID     string `json:"batchId"`
	InstanceID  string `json:"instanceId"`
	FromVersion string `json:"fromVersion,omitempty"`
	ToVersion   string `json:"toVersion,omitempty"`
	// ResolveVersion is the version the health result belongs to. A late result
	// is only applied while the member still targets this version+batch.
	ResolveVersion string `json:"resolveVersion,omitempty"`
	DueAt          string `json:"dueAt"`
	Attempt        int    `json:"attempt"`
}

type Event struct {
	Seq        int64  `json:"seq"`
	At         string `json:"at"`
	ReleaseID  string `json:"releaseId,omitempty"`
	BatchID    string `json:"batchId,omitempty"`
	InstanceID string `json:"instanceId,omitempty"`
	Level      string `json:"level"` // info|warn|error
	Message    string `json:"message"`
}

type Snapshot struct {
	Services    []Service    `json:"services"`
	CompatRules []CompatRule `json:"compatRules"`
	Versions    []Version    `json:"versions"`
	Instances   []Instance   `json:"instances"`
	Releases    []Release    `json:"releases"`
	Batches     []Batch      `json:"batches"`
	Ops         []Op         `json:"ops"`
	Events      []Event      `json:"events"`
}
