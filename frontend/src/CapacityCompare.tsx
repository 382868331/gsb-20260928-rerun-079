import { useEffect, useMemo, useState } from "react";
import { Download, GitCompareArrows } from "lucide-react";
import { compareCapacity } from "./api";
import type { CapacityComparison, Release, ReleaseCapacityView, Snapshot } from "./types";

// CapacityCompareView is the cross-release capacity comparison page. It only
// reads the persisted snapshot and the read-only compare endpoint: viewing
// history or exporting never acquires commitments, never releases instances
// and never changes the state of any in-flight release.

interface Filters {
  zoneId: string;
  serviceId: string;
  status: string;
  from: string;
  to: string;
  diffOnly: boolean;
}

const defaultFilters: Filters = { zoneId: "", serviceId: "", status: "", from: "", to: "", diffOnly: false };

const FILTER_KEY = "capacity-compare-filters";
const SELECT_KEY = "capacity-compare-selection";

function loadJSON<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return fallback;
    return { ...fallback, ...JSON.parse(raw) };
  } catch {
    return fallback;
  }
}

const terminalStatuses = ["succeeded", "failed", "rolled_back", "cancelled"];

const statusLabel: Record<string, string> = {
  pending: "等待开始",
  in_progress: "进行中",
  paused: "暂停待审查",
  cancelling: "取消中",
  rolling_back: "回退中",
  succeeded: "发布成功",
  failed: "失败",
  rolled_back: "失败已回退",
  cancelled: "已取消",
};

function timeShort(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString("zh-CN", { hour12: false });
}

export function CapacityCompareView({ snap }: { snap: Snapshot }) {
  // Filters persist across reopen: saved to localStorage on every change.
  const [filters, setFilters] = useState<Filters>(() => loadJSON(FILTER_KEY, defaultFilters));
  const [sel, setSel] = useState<{ a: string; b: string }>(() => loadJSON(SELECT_KEY, { a: "", b: "" }));
  const [cmp, setCmp] = useState<CapacityComparison | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    localStorage.setItem(FILTER_KEY, JSON.stringify(filters));
  }, [filters]);
  useEffect(() => {
    localStorage.setItem(SELECT_KEY, JSON.stringify(sel));
  }, [sel]);

  const setF = (patch: Partial<Filters>) => setFilters((f) => ({ ...f, ...patch }));

  const instZone = useMemo(() => {
    const m = new Map<string, string>();
    for (const i of snap.instances) m.set(i.id, i.zoneId);
    return m;
  }, [snap.instances]);

  const filtered = useMemo(() => {
    return snap.releases.filter((r) => {
      if (filters.serviceId && r.serviceId !== filters.serviceId) return false;
      if (filters.status === "active") {
        if (terminalStatuses.includes(r.status)) return false;
      } else if (filters.status && r.status !== filters.status) {
        return false;
      }
      if (filters.from && r.createdAt < new Date(filters.from).toISOString()) return false;
      if (filters.to && r.createdAt > new Date(filters.to).toISOString()) return false;
      if (filters.zoneId) {
        const touchesGrant = snap.grants.some((g) => g.releaseId === r.id && g.zoneId === filters.zoneId);
        const touchesMember = snap.batches.some(
          (b) => b.releaseId === r.id && b.members.some((m) => instZone.get(m.instanceId) === filters.zoneId)
        );
        if (!touchesGrant && !touchesMember) return false;
      }
      return true;
    });
  }, [snap, filters, instZone]);

  // Reload the comparison whenever the selection or the underlying snapshot
  // changes; both are read-only fetches.
  useEffect(() => {
    if (!sel.a) {
      setCmp(null);
      return;
    }
    let cancelled = false;
    compareCapacity(sel.a, sel.b || undefined)
      .then((c) => {
        if (!cancelled) {
          setCmp(c);
          setError("");
        }
      })
      .catch((e) => {
        if (!cancelled) setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, [sel.a, sel.b, snap]);

  const pick = (slot: "a" | "b", id: string) =>
    setSel((s) => ({ ...s, [slot]: s[slot] === id ? "" : id }));

  const exportJSON = () => {
    const payload = {
      exportedAt: new Date().toISOString(),
      note: "只读导出：不取得承诺、不释放实例、不改变发布状态",
      filters,
      releases: filtered.map((r) => ({
        id: r.id,
        serviceId: r.serviceId,
        targetVersion: r.targetVersion,
        status: r.status,
        createdAt: r.createdAt,
        finishedAt: r.finishedAt,
      })),
      comparison: cmp,
    };
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "capacity-compare-" + new Date().toISOString().replace(/[:.]/g, "-") + ".json";
    a.click();
    URL.revokeObjectURL(url);
  };

  const a = cmp?.releases[0];
  const b = cmp?.releases[1];

  return (
    <section className="card compare">
      <h2><GitCompareArrows size={18} /> 跨发布容量对比</h2>
      <p className="sub">
        只读视图：按区域、服务、发布时间与结束状态筛选历史发布，选择两次发布并排比较容量承诺的占用时段、
        等待时长、逐实例归还次序与最低可用策略。查看与导出均不会取得承诺、释放实例或改变发布状态。
      </p>

      <div className="cmp-filters">
        <label>故障区域
          <select value={filters.zoneId} onChange={(e) => setF({ zoneId: e.target.value })}>
            <option value="">全部区域</option>
            {snap.zones.map((z) => <option key={z.id} value={z.id}>{z.name}</option>)}
          </select>
        </label>
        <label>服务
          <select value={filters.serviceId} onChange={(e) => setF({ serviceId: e.target.value })}>
            <option value="">全部服务</option>
            {snap.services.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
        </label>
        <label>结束状态
          <select value={filters.status} onChange={(e) => setF({ status: e.target.value })}>
            <option value="">全部状态</option>
            <option value="active">未结束</option>
            <option value="succeeded">发布成功</option>
            <option value="failed">失败</option>
            <option value="rolled_back">失败已回退</option>
            <option value="cancelled">已取消</option>
          </select>
        </label>
        <label>发布时间从
          <input type="datetime-local" value={filters.from} onChange={(e) => setF({ from: e.target.value })} />
        </label>
        <label>到
          <input type="datetime-local" value={filters.to} onChange={(e) => setF({ to: e.target.value })} />
        </label>
        <div className="actions">
          <button onClick={() => setF(defaultFilters)}>重置筛选</button>
          <button onClick={exportJSON} disabled={filtered.length === 0}>
            <Download size={14} /> 导出当前筛选结果
          </button>
        </div>
      </div>
      <p className="hint">筛选条件已自动保存，重新打开页面后继续使用。</p>

      <table className="cmp-releases">
        <thead>
          <tr><th>对比 A</th><th>对比 B</th><th>发布</th><th>服务</th><th>目标版本</th><th>状态</th><th>发布时间</th><th>结束时间</th></tr>
        </thead>
        <tbody>
          {filtered.map((r: Release) => (
            <tr key={r.id} className={r.id === sel.a || r.id === sel.b ? "selected" : ""}>
              <td><input type="checkbox" checked={sel.a === r.id} onChange={() => pick("a", r.id)} /></td>
              <td><input type="checkbox" checked={sel.b === r.id} onChange={() => pick("b", r.id)} /></td>
              <td><code>{r.id}</code></td>
              <td>{snap.services.find((s) => s.id === r.serviceId)?.name ?? r.serviceId}</td>
              <td><code>{r.targetVersion}</code></td>
              <td><span className={"pill r-" + r.status}>{statusLabel[r.status] ?? r.status}</span></td>
              <td>{timeShort(r.createdAt)}</td>
              <td>{timeShort(r.finishedAt)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {filtered.length === 0 && <p className="empty">当前筛选条件下没有发布记录。</p>}
      {error && <p className="error">{error}</p>}

      {a && (
        <div className="cmp-view">
          <label className="check">
            <input type="checkbox" checked={filters.diffOnly} onChange={(e) => setF({ diffOnly: e.target.checked })} />
            只看差异
          </label>
          <CompareSection
            title="容量承诺占用时段"
            a={a} b={b}
            diffOnly={filters.diffOnly}
            rows={(r) =>
              r.grants.map((g) => ({
                key: g.zoneName + "·批" + (g.batchIndex + 1) + "·" + g.instances.map((i) => i.instanceId + "@" + i.toVersion).sort().join(","),
                text:
                  g.zoneName + " · 第 " + (g.batchIndex + 1) + " 批 · " +
                  timeShort(g.acquiredAt) + " → " + (g.state === "held" ? "持有中" : timeShort(g.releasedAt)) +
                  " · 占用 " + (g.instances.map((i) => i.instanceId + "@" + i.toVersion).join("、") || "无") +
                  (g.releaseWhy ? "（" + g.releaseWhy + "）" : ""),
              }))
            }
          />
          <CompareSection
            title="容量等待时长"
            a={a} b={b}
            diffOnly={filters.diffOnly}
            empty="两次发布均未发生容量等待"
            rows={(r) =>
              r.waits.map((w) => ({
                key: "批" + (w.batchIndex + 1) + "·" + (w.waiting ? "等待中" : "已结束") + "·" + w.reason,
                text:
                  "第 " + (w.batchIndex + 1) + " 批 · 等待 " + w.seconds + " 秒" +
                  (w.waiting ? "（仍在等待）" : "") + " · " + w.reason,
              }))
            }
          />
          <CompareSection
            title="逐实例归还次序（同一实例不同版本分开展示）"
            a={a} b={b}
            diffOnly={filters.diffOnly}
            rows={(r) =>
              r.grants
                .flatMap((g) => g.instances)
                .filter((i) => i.released)
                .sort((x, y) => (x.releaseOrder ?? 0) - (y.releaseOrder ?? 0))
                .map((i) => ({
                  key: "#" + (i.releaseOrder ?? "-") + " " + i.instanceId + "@" + i.toVersion,
                  text:
                    "#" + (i.releaseOrder ?? "-") + " " + i.instanceId + " @" + i.toVersion +
                    "（自 " + i.fromVersion + "）· " + timeShort(i.releasedAt) + " 归还",
                }))
            }
          />
          <CompareSection
            title="最低可用策略"
            a={a} b={b}
            diffOnly={filters.diffOnly}
            rows={(r) =>
              r.policies.map((p) => ({
                key: p.zoneName + "·min=" + (p.hasMin ? p.minAvailable : "无") + "·shared=" + (p.sharedCapacity || "不限"),
                text:
                  p.zoneName + " · " + r.serviceName + "最低可用 " + (p.hasMin ? p.minAvailable + " 台" : "无限制") +
                  " · 跨服务共享容量 " + (p.sharedCapacity > 0 ? p.sharedCapacity + " 台" : "不限") +
                  (p.basis.length > 0 ? " · 依据：" + p.basis.join("；") : ""),
              }))
            }
          />
          <p className="hint">对比数据生成于 {timeShort(cmp?.generatedAt)}，来自持久化记录，不影响任何进行中的发布。</p>
        </div>
      )}
      {!a && <p className="hint">在上方列表勾选“对比 A”（可再勾选“对比 B”）后，这里并排展示两次发布的容量差异。</p>}
    </section>
  );
}

interface Row {
  key: string;
  text: string;
}

function CompareSection({
  title,
  a,
  b,
  rows,
  diffOnly,
  empty = "两侧均无记录",
}: {
  title: string;
  a: ReleaseCapacityView;
  b?: ReleaseCapacityView;
  rows: (r: ReleaseCapacityView) => Row[];
  diffOnly: boolean;
  empty?: string;
}) {
  const ra = rows(a);
  const rb = b ? rows(b) : [];
  const n = Math.max(ra.length, rb.length);
  const pairs = Array.from({ length: n }, (_, i) => {
    const left = ra[i];
    const right = rb[i];
    const same = !!b && !!left && !!right && left.key === right.key;
    return { left, right, same };
  });
  const shown = diffOnly && b ? pairs.filter((p) => !p.same) : pairs;
  return (
    <div className="cmp-section">
      <h3>{title}</h3>
      {n === 0 && <p className="empty">{empty}</p>}
      {n > 0 && shown.length === 0 && <p className="empty">两侧完全一致，无差异。</p>}
      {shown.length > 0 && (
        <div className={"cmp-grid" + (b ? "" : " single")}>
          <div className="cmp-col-head">A · {a.releaseId}（{a.serviceName} → {a.targetVersion}）</div>
          {b && <div className="cmp-col-head">B · {b.releaseId}（{b.serviceName} → {b.targetVersion}）</div>}
          {shown.map((p, i) => (
            <div key={i} className={"cmp-row" + (b ? (p.same ? " same" : " diff") : "")}>
              <div className="cmp-cell">{p.left?.text ?? "—"}</div>
              {b && <div className="cmp-cell">{p.right?.text ?? "—"}</div>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
