import type { CapacityComparison, CompatRule, FailureSpec, Instance, Plan, Snapshot } from "./types";

// normalizeSnapshot defends the render path against null collections: an
// empty drill must render its empty state, never crash on a null lookup.
export function normalizeSnapshot(s: Snapshot): Snapshot {
  return {
    ...s,
    versions: s.versions ?? [],
    services: s.services ?? [],
    zones: s.zones ?? [],
    rules: s.rules ?? [],
    instances: s.instances ?? [],
    releases: s.releases ?? [],
    batches: s.batches ?? [],
    grants: s.grants ?? [],
    ops: s.ops ?? [],
    events: s.events ?? [],
    dependencies: s.dependencies ?? [],
  };
}

export async function fetchSnapshot(signal?: AbortSignal): Promise<Snapshot> {
  const res = await fetch("/api/state", { signal });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export async function createRelease(req: {
  serviceId?: string;
  targetVersion: string;
  batchSize: number;
  strategy: string;
  failure: FailureSpec;
}): Promise<void> {
  const res = await fetch("/api/releases", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "创建发布失败");
  }
}

export async function cancelRelease(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/cancel`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "取消发布失败");
  }
}

export async function resumeRollback(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/resume`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "继续回退失败");
  }
}

export async function finishDrill(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/finish`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "结束演练失败");
  }
}

export async function initEnvironment(): Promise<Snapshot> {
  const res = await fetch("/api/environment/init", { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "初始化演练环境失败");
  }
  return res.json();
}

export async function addInstance(serviceId: string): Promise<Instance> {
  const res = await fetch("/api/services/" + serviceId + "/instances", { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "新增虚拟实例失败");
  }
  return res.json();
}

export async function addRule(rule: Omit<CompatRule, "id">): Promise<CompatRule> {
  const res = await fetch("/api/rules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(rule),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "新增兼容约束失败");
  }
  return res.json();
}

export async function pauseForReview(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/pause`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "暂停发布失败");
  }
}

export async function reviewContinue(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/review/continue`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "继续发布失败");
  }
}

export async function reviewShrink(id: string, batchSize: number): Promise<void> {
  const res = await fetch(`/api/releases/${id}/review/shrink`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ batchSize }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "缩小批次失败");
  }
}

export async function reviewRollback(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/review/rollback`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "审查回退失败");
  }
}
export async function setZoneCapacity(

  zoneId: string,
  minAvailable: Record<string, number>,
  sharedCapacity: number
): Promise<void> {
  const res = await fetch("/api/zones/" + zoneId + "/capacity", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ minAvailable, sharedCapacity }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "保存区域容量配置失败");
  }
}

export async function setZoneFailure(zoneId: string, failed: boolean): Promise<void> {
  const res = await fetch("/api/zones/" + zoneId + "/failure", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ failed }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "区域故障操作失败");
  }
}

export async function comparePlans(serviceId: string, batchSize: number): Promise<Plan[]> {
  const res = await fetch("/api/plans/compare", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ serviceId, batchSize }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "比较推进方案失败");
  }
  const data = await res.json();
  return data.plans ?? [];
}

// compareCapacity fetches the read-only cross-release capacity comparison.
// It is a pure GET: it never acquires commitments, releases instances or
// changes release state.
export async function compareCapacity(a: string, b?: string): Promise<CapacityComparison> {
  const params = new URLSearchParams({ a });
  if (b) params.set("b", b);
  const res = await fetch("/api/capacity/compare?" + params.toString());
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "加载容量对比失败");
  }
  return res.json();
}

// Subscribe to live state over SSE. Falls back to polling if the stream
// cannot be opened. Either way the server payload is the persisted snapshot,
// so a page refresh always shows the real on-disk progress.
export function subscribe(onState: (s: Snapshot) => void): () => void {
  let stopped = false;
  let pollTimer: number | undefined;
  let es: EventSource | undefined;

  const poll = () => {
    fetchSnapshot()
      .then((s) => onState(normalizeSnapshot(s)))
      .catch(() => {});
  };

  try {
    es = new EventSource("/api/events");
    es.onmessage = (ev) => {
      if (stopped) return;
      try {
        onState(normalizeSnapshot(JSON.parse(ev.data)));
      } catch {
        es?.close();
        if (!stopped) pollTimer = window.setInterval(poll, 1000);
      }
    };
    es.onerror = () => {
      es?.close();
      if (!stopped) pollTimer = window.setInterval(poll, 1000);
    };
  } catch {
    pollTimer = window.setInterval(poll, 1000);
  }

  return () => {
    stopped = true;
    es?.close();
    if (pollTimer) window.clearInterval(pollTimer);
  };
}
