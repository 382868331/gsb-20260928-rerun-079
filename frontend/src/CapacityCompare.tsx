import { useEffect, useMemo, useState } from "react";
import { Download, ListFilter, Scale } from "lucide-react";
import { fetchCapacityCompare, type CapacityCompareFilters } from "./api";
import type { CapacityCompareResult, ReleaseCapacity, Snapshot } from "./types";

const FILTERS_KEY = "capacity-compare-filters-v1";

interface PersistedCompare extends CapacityCompareFilters {
  diffOnly: boolean;
  selected: string[];
}

const defaultFilters: PersistedCompare = {
  zoneId: "",
  serviceId: "",
  status: "",
  from: "",
  to: "",
  diffOnly: false,
  selected: [],
};

// Filter conditions are persisted locally so a reopen keeps the exact same
// view; they never touch server-side release state.
function loadPersisted(): PersistedCompare {
  try {
    const raw = window.localStorage.getItem(FILTERS_KEY);
    if (raw) return { ...defaultFilters, ...JSON.parse(raw) };
  } catch {
    // ignore corrupted local state
  }
  return defaultFilters;
}

function timeShort(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString("zh-CN", { hour12: false });
}

function dur(sec: number): string {
  if (!sec || sec <= 0) return "0 秒";
  if (sec < 60) return sec.toFixed(1) + " 秒";
  return (sec / 60).toFixed(1) + " 分钟";
}

const statusLabel: Record<string, string> = {
  pending: "等待开始",
  in_progress: "进行中",
  paused: "暂停待审查",
  succeeded: "发布成功",
  failed: "失败",
  cancelling: "取消中",
  rolling_back: "回退中",
  rolled_back: "失败已回退",
  cancelled: "已取消",
};

// CapacityCompare is the cross-release capacity history view. Everything it
// shows comes from the read-only /api/capacity/compare endpoint: viewing
// history and exporting never acquire commitments, release instances, or
// change any release state.
export function CapacityCompare({ snap, onError }: { snap: Snapshot; onError: (m: string) => void }) {
  const [persisted, setPersisted] = useState<PersistedCompare>(loadPersisted);
  const [result, setResult] = useState<CapacityCompareResult | null>(null);

  const update = (patch: Partial<PersistedCompare>) => {
    setPersisted((prev) => {
      const next = { ...prev, ...patch };
      try {
        window.localStorage.setItem(FILTERS_KEY, JSON.stringify(next));
      } catch {
        // storage unavailable: filters still work for this session
      }
      return next;
    });
  };

  const filters: CapacityCompareFilters = {
    zoneId: persisted.zoneId,
    serviceId: persisted.serviceId,
    status: persisted.status,
    from: persisted.from,
    to: persisted.to,
  };

  useEffect(() => {
    let cancelled = false;
    fetchCapacityCompare(filters)
      .then((r) => {
        if (!cancelled) setResult(r);
      })
      .catch((e) => onError((e as Error).message));
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [persisted.zoneId, persisted.serviceId, persisted.status, persisted.from, persisted.to]);

  const releases = useMemo(() => result?.releases ?? [], [result]);
  const selected = useMemo(
    () => persisted.selected.map((id) => releases.find((r) => r.releaseId === id)).filter(Boolean) as ReleaseCapacity[],
    [persisted.selected, releases]
  );

  const toggleSelect = (id: string) => {
    const cur = persisted.selected;
    if (cur.includes(id)) {
      update({ selected: cur.filter((x) => x !== id) });
    } else {
      update({ selected: [...cur, id].slice(-2) });
    }
  };

  const exportJson = () => {
    if (!result) return;
    const blob = new Blob([JSON.stringify(result, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "capacity-compare-" + result.generatedAt.replace(/[:.]/g, "-") + ".json";
    a.click();
    URL.revokeObjectURL(url);
  };

  // Keys used by "只看差异": rows whose key exists with identical content on
  // both sides are hidden. Occupancy keys include the version transition, so
  // the same instance occupied under different versions stays a separate row.
  const keyOf = {
    grant: (r: ReleaseCapacity, i: number) => {
      const g = r.grants[i];
      return [g.zoneName, g.batchIndex, g.state, g.instances.join(",")].join("|");
    },
    ret: (r: ReleaseCapacity, i: number) => {
      const t = r.returns[i];
      return [t.order, t.instanceId].join("|");
    },
    occ: (r: ReleaseCapacity, i: number) => {
      const o = r.occupancy[i];
      return [o.instanceId, o.fromVersion, o.toVersion].join("|");
    },
    pol: (r: ReleaseCapacity, i: number) => {
      const p = r.policies[i];
      return [p.zoneId, p.minAvailable, p.sharedCapacity].join("|");
    },
  };
  const bothSelected = selected.length === 2;
  const sharedKeys = (section: keyof typeof keyOf): Set<string> => {
    if (!bothSelected || !persisted.diffOnly) return new Set();
    const [a, b] = selected;
    const keysA = new Set(a[section === "grant" ? "grants" : section === "ret" ? "returns" : section === "occ" ? "occupancy" : "policies"].map((_, i) => keyOf[section](a, i)));
    const shared = new Set<string>();
    const listB = b[section === "grant" ? "grants" : section === "ret" ? "returns" : section === "occ" ? "occupancy" : "policies"];
    listB.forEach((_, i) => {
      const k = keyOf[section](b, i);
      if (keysA.has(k)) shared.add(k);
    });
    return shared;
  };
  const shared = {
    grant: sharedKeys("grant"),
    ret: sharedKeys("ret"),
    occ: sharedKeys("occ"),
    pol: sharedKeys("pol"),
  };
  const hidden = (section: keyof typeof shared, r: ReleaseCapacity, i: number) =>
    shared[section].has(keyOf[section](r, i));

  return (
    <section className="card compare">
      <h2><Scale size={18} /> 跨发布容量对比</h2>
      <p className="sub">
        按区域、服务、发布时间与结束状态筛选历史发布，选择两次发布并排比较容量承诺的占用时段、等待时长、逐实例归还次序与最低可用策略。
        查看与导出均为只读，不会重新取得承诺、释放实例或改变任何发布状态。筛选条件保存在本地，重新打开页面后继续使用。
      </p>

      <div className="cmp-filters">
        <label>故障区域
          <select value={persisted.zoneId} onChange={(e) => update({ zoneId: e.target.value })}>
            <option value="">全部区域</option>
            {snap.zones.map((z) => <option key={z.id} value={z.id}>{z.name}</option>)}
          </select>
        </label>
        <label>服务
          <select value={persisted.serviceId} onChange={(e) => update({ serviceId: e.target.value })}>
            <option value="">全部服务</option>
            {snap.services.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
        </label>
        <label>结束状态
          <select value={persisted.status} onChange={(e) => update({ status: e.target.value })}>
            <option value="">全部状态</option>
            <option value="ended">已结束（成功/失败/回退/取消）</option>
            <option value="succeeded">发布成功</option>
            <option value="failed">失败</option>
            <option value="rolled_back">失败已回退</option>
            <option value="cancelled">已取消</option>
            <option value="in_progress">进行中</option>
            <option value="paused">暂停待审查</option>
          </select>
        </label>
        <label>发布时间从
          <input type="date" value={persisted.from} onChange={(e) => update({ from: e.target.value })} />
        </label>
        <label>到
          <input type="date" value={persisted.to} onChange={(e) => update({ to: e.target.value })} />
        </label>
        <div className="actions">
          <button onClick={exportJson} disabled={!result || releases.length === 0}>
            <Download size={15} /> 导出当前筛选结果
          </button>
        </div>
      </div>

      {releases.length === 0 ? (
        <p className="empty"><ListFilter size={14} /> 当前筛选条件下没有发布记录。调整筛选条件或先完成一次演练发布。</p>
      ) : (
        <table className="cmp-list">
          <thead>
            <tr><th>对比</th><th>发布</th><th>服务</th><th>版本</th><th>状态</th><th>发布时间</th><th>承诺数</th><th>累计等待</th></tr>
          </thead>
          <tbody>
            {releases.map((r) => (
              <tr key={r.releaseId} className={persisted.selected.includes(r.releaseId) ? "selected" : ""}>
                <td>
                  <input
                    type="checkbox"
                    checked={persisted.selected.includes(r.releaseId)}
                    onChange={() => toggleSelect(r.releaseId)}
                  />
                </td>
                <td><code>{r.releaseId}</code></td>
                <td>{r.serviceName || r.serviceId}</td>
                <td><code>{r.baseline} → {r.targetVersion}</code></td>
                <td><span className={"pill r-" + r.status}>{statusLabel[r.status] ?? r.status}</span></td>
                <td>{timeShort(r.createdAt)}</td>
                <td>{r.grants.length}</td>
                <td>{dur(r.totalWaitSeconds)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {selected.length > 0 && (
        <>
          <div className="cmp-toolbar">
            <label className="check">
              <input
                type="checkbox"
                checked={persisted.diffOnly}
                disabled={!bothSelected}
                onChange={(e) => update({ diffOnly: e.target.checked })}
              />
              只看差异{!bothSelected && "（选择两次发布后可用）"}
            </label>
          </div>
          <div className={"cmp-cols n" + selected.length}>
            {selected.map((r) => (
              <ReleaseColumn
                key={r.releaseId}
                r={r}
                hidden={(section, i) => hidden(section, r, i)}
              />
            ))}
          </div>
        </>
      )}
    </section>
  );
}

function ReleaseColumn({
  r,
  hidden,
}: {
  r: ReleaseCapacity;
  hidden: (section: "grant" | "ret" | "occ" | "pol", i: number) => boolean;
}) {
  const grants = r.grants.filter((_, i) => !hidden("grant", i));
  const returns = r.returns.filter((_, i) => !hidden("ret", i));
  const occupancy = r.occupancy.filter((_, i) => !hidden("occ", i));
  const policies = r.policies.filter((_, i) => !hidden("pol", i));
  return (
    <div className="cmp-col">
      <h3>
        <code>{r.releaseId}</code> {r.serviceName || r.serviceId}
        <span className={"pill r-" + r.status}>{statusLabel[r.status] ?? r.status}</span>
      </h3>
      <p className="sub">
        <code>{r.baseline} → {r.targetVersion}</code> · 发布于 {timeShort(r.createdAt)}
        {r.finishedAt && <> · 结束于 {timeShort(r.finishedAt)}</>}
      </p>

      <h4>容量承诺占用时段（{grants.length}）</h4>
      {grants.length === 0 && <p className="empty">无容量承诺记录。</p>}
      <ul className="cmp-rows">
        {grants.map((g) => (
          <li key={g.grantId} className={g.state === "held" ? "held" : "released"}>
            <strong>{g.zoneName || g.zoneId}</strong> 第 {g.batchIndex + 1} 批 ·{" "}
            {g.state === "held" ? "持有中" : "已释放"}：{timeShort(g.acquiredAt)} →{" "}
            {g.state === "held" ? "至今" : timeShort(g.releasedAt)}（占用 {dur(g.occupySeconds)}）
            {g.waitSeconds > 0 && <span className="hint-inline"> · 取得前等待 {dur(g.waitSeconds)}</span>}
            {g.releaseWhy && <div className="hint">{g.releaseWhy}</div>}
          </li>
        ))}
      </ul>

      <h4>等待时长</h4>
      <p className="hint-inline">累计等待容量 {dur(r.totalWaitSeconds)}</p>
      {r.waiting.length > 0 && (
        <ul className="cmp-rows">
          {r.waiting.map((w) => (
            <li key={w.batchId} className="held">
              第 {w.batchIndex + 1} 批仍在等待（自 {timeShort(w.since)}）：{w.reason}
            </li>
          ))}
        </ul>
      )}

      <h4>逐实例归还次序（{returns.length}）</h4>
      {returns.length === 0 && <p className="empty">暂无实例归还记录。</p>}
      <ol className="cmp-rows">
        {returns.map((t) => (
          <li key={t.order + "-" + t.instanceId}>
            <code>{t.instanceId}</code>
            {t.zoneName && <>（{t.zoneName}）</>} · {timeShort(t.at)}
            {t.why && <div className="hint">{t.why}</div>}
          </li>
        ))}
      </ol>

      <h4>占用实例（按版本分开，{occupancy.length}）</h4>
      {occupancy.length === 0 && <p className="empty">无占用实例。</p>}
      <ul className="cmp-rows">
        {occupancy.map((o, i) => (
          <li key={o.instanceId + o.toVersion + i}>
            <code>{o.instanceId}</code> <code>{o.fromVersion} → {o.toVersion}</code>
            {o.zoneName && <>（{o.zoneName}）</>} · 第 {o.batchIndex + 1} 批
          </li>
        ))}
      </ul>

      <h4>最低可用策略（{policies.length}）</h4>
      {policies.length === 0 && <p className="empty">未涉及任何故障区域策略。</p>}
      <ul className="cmp-rows">
        {policies.map((p) => (
          <li key={p.zoneId}>
            <strong>{p.zoneName || p.zoneId}</strong>：本服务最低可用 {p.minAvailable} 台 ·
            跨服务共享容量 {p.sharedCapacity > 0 ? p.sharedCapacity + " 台" : "不限"}
          </li>
        ))}
      </ul>
    </div>
  );
}
