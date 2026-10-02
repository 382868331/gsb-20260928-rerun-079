// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { App } from "./App";

const empty = { versions: [], instances: [], releases: [], batches: [], ops: [], events: [] };

function stubEmpty(payload: unknown) {
  vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, text: async () => "", json: async () => payload })));
  class ES {
    static CONNECTING = 0;
    onmessage: ((ev: MessageEvent) => void) | null = null;
    onerror: (() => void) | null = null;
    close() {}
    constructor() {
      setTimeout(() => this.onmessage?.({ data: JSON.stringify(payload) } as MessageEvent), 0);
    }
  }
  vi.stubGlobal("EventSource", ES);
}

describe("empty data entry", () => {
  it("renders empty snapshot without crashing", async () => {
    stubEmpty(empty);
    const { container } = render(<App />);
    await waitFor(() => expect(container.textContent).toContain("软件发布演练平台"));
    expect(container.textContent).toContain("初始化演练环境");
    expect(container.textContent).toContain("暂无版本");
    expect(container.textContent).toContain("暂无发布记录");
  });

  it("tolerates null collections from a fresh data directory", async () => {
    // A fresh backend serialized empty slices as null; the first render must
    // not die on a null lookup and trap the page on the failure screen.
    stubEmpty({ services: null, compatRules: null, versions: null, instances: null, releases: null, batches: null, ops: null, events: null });
    const { container } = render(<App />);
    await waitFor(() => expect(container.textContent).toContain("初始化演练环境"));
    expect(container.textContent).not.toContain("页面渲染失败");
  });

  it("shows compat evidence and blocked reason on a waiting batch", async () => {
    const snap = {
      services: [{ id: "order", name: "订单服务" }, { id: "payment", name: "支付服务" }],
      compatRules: [{ id: "r1", dependentId: "payment", dependencyId: "order", mode: "dependent_first", note: "支付必须先更新" }],
      versions: [
        { id: "v1.0.0", serviceId: "order", name: "订单 1.0.0", artifactRef: "local://v1", createdAt: "2026-09-29T00:00:00Z" },
        { id: "v2.0.0", serviceId: "order", name: "订单 2.0.0", artifactRef: "local://v2", createdAt: "2026-09-29T00:00:00Z" },
      ],
      instances: [
        { id: "inst-1", serviceId: "order", name: "订单实例-1", currentVersion: "v1.0.0", desiredVersion: "v1.0.0", health: "healthy", updatedAt: "2026-09-29T00:00:00Z" },
      ],
      releases: [{
        id: "rel-1", serviceId: "order", targetVersion: "v2.0.0", baseline: "v1.0.0", strategy: "rolling",
        batchSize: 1, batchIds: ["b1"], status: "in_progress",
        failure: { mode: "none" }, cancelRequested: false, resumeRequested: false, finishRequested: false,
        createdAt: "2026-09-29T00:00:00Z",
        message: "等待兼容约束满足：依赖方 支付服务 必须先完成更新",
      }],
      batches: [{
        id: "b1", releaseId: "rel-1", index: 0, phase: "pending", targetVersion: "v2.0.0",
        members: [],
        blockedReason: "等待兼容约束满足：依赖方 支付服务 必须先完成更新：0/2 实例已在 pay-v2.0.0（未满足）",
        compat: [
          { serviceId: "payment", kind: "compat", ok: false, detail: "依赖方 支付服务 必须先完成更新：0/2 实例已在 pay-v2.0.0" },
          { serviceId: "order", kind: "health", ok: true, detail: "订单服务 健康依据：1/1 实例健康" },
        ],
      }],
      ops: [],
      events: [],
    };
    stubEmpty(snap);
    const { container } = render(<App />);
    await waitFor(() => expect(container.textContent).toContain("等待兼容约束满足"));
    expect(container.textContent).toContain("依赖方 支付服务 必须先完成更新");
    expect(container.textContent).toContain("健康依据");
    expect(container.textContent).toContain("依赖者先更新");
  });
});
