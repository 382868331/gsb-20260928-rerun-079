import type { FailureSpec, Snapshot } from "./types";

export async function fetchSnapshot(signal?: AbortSignal): Promise<Snapshot> {
  const res = await fetch("/api/state", { signal });
  if (!res.ok) throw new Error(await res.text());
  return normalize(await res.json());
}

// Older servers or hand-written data files may emit null for empty
// collections; the UI always works with arrays.
function normalize(s: Snapshot): Snapshot {
  s.versions ??= [];
  s.instances ??= [];
  s.releases ??= [];
  s.batches ??= [];
  s.ops ??= [];
  s.events ??= [];
  return s;
}

export async function createRelease(req: {
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

export async function pauseRelease(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/pause`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "请求审查暂停失败");
  }
}

export async function continueRelease(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/continue`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "继续发布失败");
  }
}

export async function shrinkRelease(id: string, batchSize: number): Promise<void> {
  const res = await fetch(`/api/releases/${id}/shrink`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ batchSize }),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "缩小批次失败");
  }
}

export async function rollbackRelease(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/rollback`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "整体回退失败");
  }
}

// Subscribe to live state over SSE. Falls back to polling if the stream
// cannot be opened. Either way the server payload is the persisted snapshot,
// so a page refresh always shows the real on-disk progress.
export function subscribe(onState: (s: Snapshot) => void, onError?: (message: string) => void): () => void {
  let stopped = false;
  let pollTimer: number | undefined;
  let es: EventSource | undefined;

  const poll = () => {
    fetchSnapshot()
      .then(onState)
      .catch((e) => {
        if (!stopped) onError?.(e instanceof Error ? e.message : String(e));
      });
  };

  try {
    es = new EventSource("/api/events");
    es.onmessage = (ev) => {
      if (!stopped) onState(normalize(JSON.parse(ev.data)));
    };
    es.onerror = () => {
      es?.close();
      if (!stopped && !pollTimer) {
        onError?.("实时连接断开，已切换为轮询；若持续失败请检查后端服务");
        pollTimer = window.setInterval(poll, 1000);
      }
    };
  } catch {
    onError?.("无法建立实时连接，已切换为轮询");
    pollTimer = window.setInterval(poll, 1000);
  }

  return () => {
    stopped = true;
    es?.close();
    if (pollTimer) window.clearInterval(pollTimer);
  };
}
