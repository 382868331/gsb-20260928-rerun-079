export interface Version {
  id: string;
  name: string;
  artifactRef: string;
  createdAt: string;
}

export interface Instance {
  id: string;
  name: string;
  serviceId: string;
  zoneId: string;
  currentVersion: string;
  desiredVersion: string;
  health: "healthy" | "unhealthy" | "unknown";
  updatedAt: string;
}

export interface Service {
  id: string;
  name: string;
}

export interface Zone {
  id: string;
  name: string;
  minAvailable: Record<string, number>;
  sharedCapacity: number;
  failed: boolean;
}

export interface CapacityGrant {
  id: string;
  releaseId: string;
  batchId: string;
  zoneId: string;
  instanceIds: string[];
  state: "held" | "released";
  acquiredAt: string;
  releasedAt?: string;
  releaseWhy?: string;
}

export interface CompatRule {
  id: string;
  providerId: string;
  dependentId: string;
  dependentVersion?: string;
  minProviderVersion?: string;
  maxProviderVersion?: string;
  mode: "update_first" | "stay_old" | "pause";
  note?: string;
}

export interface CompatFinding {
  ruleId: string;
  providerId: string;
  dependentId: string;
  mode: string;
  compatible: boolean;
  blocking: boolean;
  detail: string;
}

export interface Preflight {
  checkedAt: string;
  compatible: boolean;
  findings: CompatFinding[];
  healthBasis: string[];
  capacityBasis: string[];
}

export interface DependencyStatus {
  releaseId: string;
  serviceId: string;
  blocked: boolean;
  pending: string[];
}

export interface MemberState {
  instanceId: string;
  fromVersion: string;
  toVersion: string;
  updateState: "pending" | "in_progress" | "succeeded" | "failed" | "rollback_in_progress" | "rolled_back";
  healthState: "pending" | "in_progress" | "healthy" | "unhealthy" | "stale";
  message: string;
  lastResolvedKey: string;
  lateHealthResult?: "healthy" | "unhealthy";
  lateHealthVersion?: string;
  lateHealthAt?: string;
}

export interface Batch {
  id: string;
  releaseId: string;
  index: number;
  phase: "pending" | "updating" | "observing" | "paused" | "succeeded" | "failed" | "cancelling" | "rolling_back" | "rolled_back" | "cancelled";
  targetVersion: string;
  members: MemberState[];
  preflight?: Preflight;
  capacityWait?: string;
  grantIds?: string[];
  startedAt?: string;
  finishedAt?: string;
}

export interface FailureSpec {
  mode: "none" | "half_batch" | "instances" | "rollback_interrupt";
  instances?: string[];
  failFirstNRollback?: number;
  pauseOnFailure?: boolean;
}

export interface Release {
  id: string;
  serviceId: string;
  targetVersion: string;
  baseline: string;
  strategy: string;
  batchSize: number;
  batchIds: string[];
  status: "pending" | "in_progress" | "paused" | "succeeded" | "failed" | "cancelling" | "rolling_back" | "rolled_back" | "cancelled";
  failure: FailureSpec;
  cancelRequested: boolean;
  resumeRequested: boolean;
  finishRequested: boolean;
  pauseRequested?: boolean;
  reviewAction?: "" | "continue" | "shrink" | "rollback" | "finish" | "cancel";
  reviewShrinkTo?: number;
  reviewAt?: string;
  compatBlocked?: boolean;
  rollbackNote?: string;
  createdAt: string;
  finishedAt?: string;
  message?: string;
}

export interface Op {
  id: string;
  kind: "update" | "health" | "rollback";
  releaseId: string;
  batchId: string;
  instanceId: string;
  fromVersion?: string;
  toVersion?: string;
  resolveVersion?: string;
  dueAt: string;
  attempt: number;
}

export interface Event {
  seq: number;
  at: string;
  releaseId?: string;
  batchId?: string;
  instanceId?: string;
  level: "info" | "warn" | "error";
  message: string;
}

export interface Snapshot {
  versions: Version[];
  services: Service[];
  zones: Zone[];
  rules: CompatRule[];
  instances: Instance[];
  releases: Release[];
  batches: Batch[];
  grants: CapacityGrant[];
  ops: Op[];
  events: Event[];
  dependencies: DependencyStatus[];
}

export interface PlanBatch {
  index: number;
  instanceIds: string[];
  zoneNames: string[];
  capacityOk: boolean;
  reason?: string;
}

export interface Plan {
  strategy: string;
  name: string;
  batches: PlanBatch[];
}
