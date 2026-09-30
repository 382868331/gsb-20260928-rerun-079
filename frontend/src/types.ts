export interface Version {
  id: string;
  name: string;
  artifactRef: string;
  createdAt: string;
}

export interface Instance {
  id: string;
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
}

export interface Batch {
  id: string;
  releaseId: string;
  index: number;
  phase: "pending" | "updating" | "observing" | "succeeded" | "failed" | "cancelling" | "rolling_back" | "rolled_back" | "cancelled";
  targetVersion: string;
  members: MemberState[];
  startedAt?: string;
  finishedAt?: string;
}

export interface FailureSpec {
  mode: "none" | "half_batch" | "instances" | "rollback_interrupt";
  instances?: string[];
  failFirstNRollback?: number;
}

export interface Release {
  id: string;
  targetVersion: string;
  baselineVersion: string;
  strategy: string;
  batchSize: number;
  batchIds: string[];
  status: "pending" | "in_progress" | "paused" | "succeeded" | "failed" | "cancelling" | "rolled_back" | "cancelled";
  failure: FailureSpec;
  cancelRequested: boolean;
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
  instances: Instance[];
  releases: Release[];
  batches: Batch[];
  ops: Op[];
  events: Event[];
}
