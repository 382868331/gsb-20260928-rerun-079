import { render, screen } from "@testing-library/react";
import { App } from "./App";
import { it, expect, vi, beforeEach } from "vitest";
import type { Snapshot } from "./types";

const base: Snapshot = {
  versions: [
    { id: "v1.0.0", name: "基线", artifactRef: "x", createdAt: "2026-09-30T00:00:00Z" },
    { id: "v2.0.0", name: "目标", artifactRef: "y", createdAt: "2026-09-30T00:00:00Z" },
  ],
  instances: [
    { id: "inst-1", name: "虚拟实例-1", currentVersion: "v2.0.0", desiredVersion: "v1.0.0", health: "unhealthy", updatedAt: "2026-09-30T00:00:00Z" },
    { id: "inst-2", name: "虚拟实例-2", currentVersion: "v2.0.0", desiredVersion: "v1.0.0", health: "unknown", updatedAt: "2026-09-30T00:00:00Z" },
    { id: "inst-3", name: "虚拟实例-3", currentVersion: "v1.0.0", desiredVersion: "v1.0.0", health: "healthy", updatedAt: "2026-09-30T00:00:00Z" },
  ],
  releases: [{
    id: "rel-1", targetVersion: "v2.0.0", baselineVersion: "v1.0.0", strategy: "rolling",
    batchSize: 2, batchIds: ["b1"], status: "paused",
    failure: { mode: "half_batch" }, cancelRequested: false, createdAt: "2026-09-30T00:00:00Z",
    message: "发布已暂停：健康观察未通过",
  }],
  batches: [{
    id: "b1", releaseId: "rel-1", index: 0, phase: "failed", targetVersion: "v2.0.0",
    members: [
      { instanceId: "inst-1", fromVersion: "v1.0.0", toVersion: "v2.0.0", updateState: "succeeded", healthState: "unhealthy", message: "健康检查失败，等待回退", lastResolvedKey: "k1" },
      { instanceId: "inst-2", fromVersion: "v1.0.0", toVersion: "v2.0.0", updateState: "succeeded", healthState: "healthy", message: "新版本健康检查通过", lastResolvedKey: "k2" },
    ],
    startedAt: "2026-09-30T00:00:00Z", finishedAt: "2026-09-30T00:01:00Z",
  }],
  ops: [],
  events: [],
};

beforeEach(() => {
  // @ts-ignore
  global.EventSource = class {
    onmessage = null; onerror = null;
    constructor() { setTimeout(() => this.onerror && (this as any).onerror(), 0); }
    close() {}
  };
  // @ts-ignore
  global.fetch = vi.fn(async () => ({ ok: true, json: async () => base, text: async () => "" }));
});

it("shows paused release with failed-instance review and decision buttons", async () => {
  const { container } = render(<App />);
  await screen.findByText("已暂停·待决策", undefined, { timeout: 3000 });
  expect(screen.getByText(/审查第 1 批的失败实例/)).toBeTruthy();
  expect(screen.getAllByText(/inst-1/).length).toBeGreaterThan(0);
  expect(screen.getByText(/继续回退失败批次/)).toBeTruthy();
  expect(screen.getByText(/结束本次演练/)).toBeTruthy();
  // Version distribution comes from real instance versions.
  expect(screen.getByText("版本分布（实时核对）")).toBeTruthy();
  expect(container.innerHTML).toContain("v2.0.0");
});
