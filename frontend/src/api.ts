import type { Snapshot } from "./types";

async function post(path: string, body?: unknown): Promise<void> {
	const res = await fetch(path, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: body ? JSON.stringify(body) : undefined,
	});
	if (!res.ok) {
		const err = await res.json().catch(() => ({ error: res.statusText }));
		throw new Error(err.error ?? "request failed");
	}
}

export async function fetchState(): Promise<Snapshot> {
	const res = await fetch("/api/state");
	if (!res.ok) throw new Error("failed to load state");
	return res.json();
}

export interface ScenarioOptions {
	base_version?: string;
	target_version?: string;
	instance_count?: number;
	fail_update_idx?: number[];
	unhealthy_idx?: number[];
	probe_delay_ms?: Record<number, number>;
}

export const api = {
	setup: (opts: ScenarioOptions) => post("/api/setup", opts),
	startRelease: (targetVersionId: string, batchPercent: number[]) =>
		post("/api/releases", {
			target_version_id: targetVersionId,
			batch_percent: batchPercent,
		}),
	cancel: (id: string) => post(`/api/releases/${id}/cancel`),
	pause: () => post("/api/pause"),
	resume: () => post("/api/resume"),
};
