import type { FailureSpec, Snapshot } from "./types";

export async function fetchSnapshot(signal?: AbortSignal): Promise<Snapshot> {
  const res = await fetch("/api/state", { signal });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
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
  await fetch(`/api/releases/${id}/cancel`, { method: "POST" });
}

export async function resumeRelease(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/resume`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "继续回退失败");
  }
}

export async function finishRelease(id: string): Promise<void> {
  const res = await fetch(`/api/releases/${id}/finish`, { method: "POST" });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || "结束演练失败");
  }
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
      .then(onState)
      .catch(() => {});
  };

  try {
    es = new EventSource("/api/events");
    es.onmessage = (ev) => {
      if (!stopped) onState(JSON.parse(ev.data));
    };
    es.onerror = () => {
      es?.close();
      if (!stopped && !pollTimer) {
        poll();
        pollTimer = window.setInterval(poll, 1000);
      }
    };
  } catch {
    poll();
    pollTimer = window.setInterval(poll, 1000);
  }

  return () => {
    stopped = true;
    es?.close();
    if (pollTimer) window.clearInterval(pollTimer);
  };
}
