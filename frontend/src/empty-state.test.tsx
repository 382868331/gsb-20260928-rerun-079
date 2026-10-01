// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { App } from "./App";

const empty = { versions: [], instances: [], releases: [], batches: [], ops: [], events: [] };

describe("empty data entry", () => {
  it("renders empty snapshot without crashing", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, text: async () => "", json: async () => empty })));
    class ES {
      static CONNECTING = 0;
      onmessage: ((ev: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close() {}
      constructor() {
        setTimeout(() => this.onmessage?.({ data: JSON.stringify(empty) } as MessageEvent), 0);
      }
    }
    vi.stubGlobal("EventSource", ES);
    const { container } = render(<App />);
    await waitFor(() => expect(container.textContent).toContain("软件发布演练平台"));
    expect(container.textContent).toContain("初始化演练环境");
    expect(container.textContent).toContain("暂无版本");
    expect(container.textContent).toContain("暂无发布记录");
  });
});
