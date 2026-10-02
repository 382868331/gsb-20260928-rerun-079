export interface Service {
  id: string;
  name: string;
}

export interface CompatRule {
  id: string;
  dependentId: string;
  dependencyId: string;
  mode: "dependent_first" | "stay_old" | "pause";
  note: string;
}

export interface CompatCheck {
  serviceId: string;
  kind: "compat" | "health";
  ok: boolean;
  detail: string;
}

export interface Version {
  id: string;
  serviceId: string;
  name: string;
  artifactRef: string;
  createdAt: string;
}

export interface Instance {
  id: string;
  serviceId: string;
  name: string;
  currentVersion: string;
  desiredVersion: string;
  health: "healthy" | "unhealthy" | "unknown";
  updatedAt: string;
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
  compat?: CompatCheck[];
  blockedReason?: string;
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
  rollbackImpact?: string[];
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
  services: Service[];
  compatRules: CompatRule[];
  versions: Version[];
  instances: Instance[];
  releases: Release[];
  batches: Batch[];
  ops: Op[];
  events: Event[];
}
