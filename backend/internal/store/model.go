package store

// Version is an immutable artifact record. Versions, instances and release
// batches are stored in separate files on purpose.
type Version struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ArtifactRef string `json:"artifactRef"`
	CreatedAt   string `json:"createdAt"`
}

// Service is a virtual service in the drill. Instances belong to exactly one
// service; releases always target a single service.
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Zone is a failure zone (故障区域). Every virtual instance belongs to
// exactly one zone; capacity limits are evaluated per zone.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// MinAvailable maps service ID to the minimum number of instances of
	// that service that must remain available (not occupied by any batch)
	// inside this zone. Missing entry means no per-service limit.
	MinAvailable map[string]int `json:"minAvailable"`
	// SharedCapacity is the maximum number of instances across all
	// services in this zone that may be occupied at the same time.
	// Zero means no shared limit.
	SharedCapacity int  `json:"sharedCapacity"`
	Failed         bool `json:"failed"`
}

// CapacityGrant is a queryable capacity commitment (容量承诺) held by one
// batch of one release inside one zone. InstanceIDs shrinks as members
// settle into a verified real state (healthy on the new version or rolled
// back to the old one); the grant is released only when it is empty.
type CapacityGrant struct {
	ID          string   `json:"id"`
	ReleaseID   string   `json:"releaseId"`
	BatchID     string   `json:"batchId"`
	ZoneID      string   `json:"zoneId"`
	InstanceIDs []string `json:"instanceIds"`
	State       string   `json:"state"` // held|released
	AcquiredAt  string   `json:"acquiredAt"`
	ReleasedAt  string   `json:"releasedAt,omitempty"`
	ReleaseWhy  string   `json:"releaseWhy,omitempty"`
}

// CompatRule describes which provider versions a dependent service can run
// against. When the provider service is released to a target outside
// [MinProviderVersion, MaxProviderVersion] while the dependent sits on
// DependentVersion (empty = any), the rule is violated and Mode decides the
// consequence:
//   - update_first: the dependent must be updated first; the provider batch
//     is blocked until then.
//   - stay_old: the dependent may stay on its old version; informational.
//   - pause: the provider release pauses for manual review before expanding.
type CompatRule struct {
	ID                 string `json:"id"`
	ProviderID         string `json:"providerId"`
	DependentID        string `json:"dependentId"`
	DependentVersion   string `json:"dependentVersion,omitempty"`
	MinProviderVersion string `json:"minProviderVersion,omitempty"`
	MaxProviderVersion string `json:"maxProviderVersion,omitempty"`
	Mode               string `json:"mode"` // update_first|stay_old|pause
	Note               string `json:"note,omitempty"`
}

// CompatFinding is one evaluated rule outcome, persisted on the batch that
// was gated by it so the basis of every decision survives a reopen.
type CompatFinding struct {
	RuleID      string `json:"ruleId"`
	ProviderID  string `json:"providerId"`
	DependentID string `json:"dependentId"`
	Mode        string `json:"mode"`
	Compatible  bool   `json:"compatible"`
	Blocking    bool   `json:"blocking"`
	Detail      string `json:"detail"`
}

// Preflight is the compatibility + health basis evaluated right before a
// batch takes effect (and re-evaluated before it is allowed to succeed).
type Preflight struct {
	CheckedAt   string          `json:"checkedAt"`
	Compatible  bool            `json:"compatible"`
	Findings    []CompatFinding `json:"findings"`
	HealthBasis []string        `json:"healthBasis"`
	// CapacityBasis records, per zone, which instances this batch will
	// occupy and the available/shared numbers the decision was based on.
	CapacityBasis []string `json:"capacityBasis"`
}

// Instance is a local virtual instance. DesiredVersion is what the fleet
// converges back to when no release is active (the known-good baseline).
type Instance struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ServiceID      string `json:"serviceId"`
	ZoneID         string `json:"zoneId"`
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
	Preflight     *Preflight    `json:"preflight,omitempty"`
	// CapacityWait is the persisted reason this batch cannot acquire zone
	// capacity yet (which release holds the conflicting commitment). It
	// survives a reopen and clears once the batch acquires its grants.
	CapacityWait string   `json:"capacityWait,omitempty"`
	GrantIDs     []string `json:"grantIds,omitempty"`
	StartedAt    string   `json:"startedAt,omitempty"`
	FinishedAt   string   `json:"finishedAt,omitempty"`
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
	// CompatBlocked marks a release frozen by an update_first compatibility
	// rule: a dependent service must be updated before any batch may expand.
	CompatBlocked bool `json:"compatBlocked,omitempty"`
	// RollbackNote records the assessment shown when the operator chose to
	// roll back: whether dependents stay compatible and which completed
	// batches can no longer be undone.
	RollbackNote string `json:"rollbackNote,omitempty"`
	CreatedAt    string `json:"createdAt"`
	FinishedAt   string `json:"finishedAt,omitempty"`
	Message      string `json:"message,omitempty"`
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
	Versions  []Version       `json:"versions"`
	Services  []Service       `json:"services"`
	Zones     []Zone          `json:"zones"`
	Rules     []CompatRule    `json:"rules"`
	Instances []Instance      `json:"instances"`
	Releases  []Release       `json:"releases"`
	Batches   []Batch         `json:"batches"`
	Grants    []CapacityGrant `json:"grants"`
	Ops       []Op            `json:"ops"`
	Events    []Event         `json:"events"`
	// Dependencies lists, per in-flight release, the compatibility
	// constraints that are not yet satisfied (unfinished dependencies).
	Dependencies []DependencyStatus `json:"dependencies"`
}

// DependencyStatus summarizes the unmet compatibility constraints of one
// release at snapshot time.
type DependencyStatus struct {
	ReleaseID string   `json:"releaseId"`
	ServiceID string   `json:"serviceId"`
	Blocked   bool     `json:"blocked"`
	Pending   []string `json:"pending"`
}
