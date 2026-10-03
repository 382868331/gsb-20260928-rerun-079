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
	// CapacityBasis records, per zone floor and shared pool, how many
	// instances this batch occupies and whether the agreed minimum
	// available capacity survives the batch. Persisted so the admission
	// decision is auditable after a reopen.
	CapacityBasis []string `json:"capacityBasis,omitempty"`
}

// Zone is a failure zone shared by instances of several services. A zone
// can be failed/recovered as part of the drill; while failed, every
// instance in it counts as unavailable capacity.
type Zone struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Failed   bool   `json:"failed"`
	FailedAt string `json:"failedAt,omitempty"`
}

// CapacityPolicy is the user-specified floor for one service in one zone:
// at least MinAvailable instances of ServiceID in ZoneID must stay
// available (healthy and not occupied by an in-flight batch) at all times.
type CapacityPolicy struct {
	ID           string `json:"id"`
	ServiceID    string `json:"serviceId"`
	ZoneID       string `json:"zoneId"`
	MinAvailable int    `json:"minAvailable"`
}

// SharedPool is a capacity floor several services jointly depend on: the
// sum of available instances of all member services in the zone must stay
// at or above MinAvailable, no matter which service is releasing.
type SharedPool struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	ZoneID       string   `json:"zoneId"`
	ServiceIDs   []string `json:"serviceIds"`
	MinAvailable int      `json:"minAvailable"`
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
	// CompatBlocked marks a release frozen by an update_first compatibility
	// rule: a dependent service must be updated before any batch may expand.
	CompatBlocked bool `json:"compatBlocked,omitempty"`
	// CapacityBlocked marks a release whose next batch cannot start because
	// starting it would drop a zone (or a shared pool) below its agreed
	// minimum available capacity. The release retries automatically once
	// other batches release their commitments; CapacityNote explains which
	// floor and which holder blocks it.
	CapacityBlocked bool   `json:"capacityBlocked,omitempty"`
	CapacityNote    string `json:"capacityNote,omitempty"`
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

// ZoneServiceCapacity is the live capacity ledger for one service in one
// zone: how many instances exist, how many are still actually occupied by
// in-flight batches, how many are unavailable for other reasons (failed
// zone, unhealthy), and the user-agreed floor.
type ZoneServiceCapacity struct {
	ZoneID       string `json:"zoneId"`
	ZoneName     string `json:"zoneName"`
	ZoneFailed   bool   `json:"zoneFailed"`
	ServiceID    string `json:"serviceId"`
	ServiceName  string `json:"serviceName"`
	Total        int    `json:"total"`
	Occupied     int    `json:"occupied"`
	Unavailable  int    `json:"unavailable"`
	Available    int    `json:"available"`
	MinAvailable int    `json:"minAvailable"`
	HasPolicy    bool   `json:"hasPolicy"`
	FloorOK      bool   `json:"floorOk"`
}

// PoolCapacity is the live ledger of one shared pool.
type PoolCapacity struct {
	PoolID       string   `json:"poolId"`
	Name         string   `json:"name"`
	ZoneID       string   `json:"zoneId"`
	ZoneName     string   `json:"zoneName"`
	ServiceNames []string `json:"serviceNames"`
	Total        int      `json:"total"`
	Occupied     int      `json:"occupied"`
	Available    int      `json:"available"`
	MinAvailable int      `json:"minAvailable"`
	FloorOK      bool     `json:"floorOk"`
}

// CapacityCommitment is the capacity one prepared-but-not-yet-effective
// batch still holds. It is derived from persisted member states, never
// from release status alone: an instance mid-rollback stays committed
// until the rollback op actually recovers it.
type CapacityCommitment struct {
	ReleaseID   string   `json:"releaseId"`
	BatchID     string   `json:"batchId"`
	BatchIndex  int      `json:"batchIndex"`
	ServiceID   string   `json:"serviceId"`
	ServiceName string   `json:"serviceName"`
	ZoneID      string   `json:"zoneId"`
	ZoneName    string   `json:"zoneName"`
	InstanceIDs []string `json:"instanceIds"`
	State       string   `json:"state"` // batch phase holding the commitment
}

// CapacityView is the queryable capacity ledger of the whole drill.
type CapacityView struct {
	Services    []ZoneServiceCapacity `json:"services"`
	Pools       []PoolCapacity        `json:"pools"`
	Commitments []CapacityCommitment  `json:"commitments"`
}

type Snapshot struct {
	Versions  []Version    `json:"versions"`
	Services  []Service    `json:"services"`
	Rules     []CompatRule `json:"rules"`
	Zones     []Zone       `json:"zones"`
	Policies  []CapacityPolicy `json:"policies"`
	Pools     []SharedPool     `json:"pools"`
	Instances []Instance   `json:"instances"`
	Releases  []Release    `json:"releases"`
	Batches   []Batch      `json:"batches"`
	Ops       []Op         `json:"ops"`
	Events    []Event      `json:"events"`
	// Dependencies lists, per in-flight release, the compatibility
	// constraints that are not yet satisfied (unfinished dependencies).
	Dependencies []DependencyStatus `json:"dependencies"`
	// Capacity is the live capacity ledger: per service+zone floors,
	// shared pools, and the commitments of prepared-but-not-yet-effective
	// batches.
	Capacity CapacityView `json:"capacity"`
}

// DependencyStatus summarizes the unmet compatibility constraints of one
// release at snapshot time.
type DependencyStatus struct {
	ReleaseID string   `json:"releaseId"`
	ServiceID string   `json:"serviceId"`
	Blocked   bool     `json:"blocked"`
	// Mutual marks two releases that wait on each other (each is blocked
	// by a constraint or capacity held by the other side). The drill can
	// only move on when the operator explicitly cancels one side or the
	// blocking condition clears.
	Mutual  bool     `json:"mutual"`
	Pending   []string `json:"pending"`
}
