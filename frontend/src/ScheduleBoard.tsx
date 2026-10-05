import { useState } from "react";
import { CalendarClock, History, ListOrdered, OctagonAlert, Timer } from "lucide-react";
import type { Snapshot } from "./types";

// ScheduleBoard is the coordination operations page: it shows the planned
// cross-release batch order, which batches are waiting and why, which
// capacity commitments are held or returned, and the persisted reason of
// every re-plan. Everything is read from the persisted snapshot, so a page
// reopen shows exactly the same state.

function timeShort(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleTimeString("zh-CN", { hour12: false });
}

const terminal = ["succeeded", "failed", "rolled_back", "cancelled"];

export function ScheduleBoard({ snap }: { snap: Snapshot }) {
  const [showHistory, setShowHistory] = useState(false);
  const latest = snap.plans[snap.plans.length - 1];

  const waiting = snap.batches.filter((b) => b.capacityWait);
  const held = snap.grants.filter((g) => g.state === "held");
  const activeReleases = snap.releases.filter((r) => !terminal.includes(r.status));

  const zoneUsage = snap.zones.map((z) => {
    const occupied = new Set<string>();
    const holders = new Set<string>();
    for (const g of held) {
      if (g.zoneId !== z.id) continue;
      g.instanceIds.forEach((id) => occupied.add(id));
      holders.add(g.releaseId);
    }
    return { zone: z, occupied: occupied.size, holders: [...holders] };
  });

  if (snap.releases.length === 0) return null;

  return (
    <section className="card schedule">
      <h2><ListOrdered size={18} /> 多发布协调调度</h2>
      <p className="sub">
        共享容量下跨发布统一排序：只有确认旧实例归还后容量才交给另一发布；进行中的更新/回退不会被抢占，
        等待时长会累积（老化）以防止低优先级发布饥饿。
      </p>

      {activeReleases.length > 0 && (
        <table>
          <thead><tr><th>发布</th><th>服务</th><th>目标</th><th>优先级</th><th>最迟完成</th><th>状态</th></tr></thead>
          <tbody>
            {activeReleases.map((r) => (
              <tr key={r.id}>
                <td><code>{r.id}</code></td>
                <td>{snap.services.find((s) => s.id === r.serviceId)?.name ?? r.serviceId}</td>
                <td><code>{r.targetVersion}</code></td>
                <td>{r.priority}</td>
                <td>{r.deadline ? timeShort(r.deadline) : "—"}</td>
                <td>{r.status}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3><Timer size={15} /> 区域容量占用（持有中的承诺）</h3>
      <table>
        <thead><tr><th>故障区域</th><th>已占用 / 共享上限</th><th>占用方</th></tr></thead>
        <tbody>
          {zoneUsage.map(({ zone, occupied, holders }) => (
            <tr key={zone.id}>
              <td>{zone.name}</td>
              <td className={zone.sharedCapacity > 0 && occupied > zone.sharedCapacity ? "bad" : ""}>
                {occupied} / {zone.sharedCapacity > 0 ? zone.sharedCapacity : "不限"}
              </td>
              <td>{holders.length > 0 ? holders.join("、") : "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>

      {waiting.length > 0 && (
        <>
          <h3><OctagonAlert size={15} /> 等待中的批次（{waiting.length}）</h3>
          <ul className="wait-list">
            {waiting.map((b) => (
              <li key={b.id}>
                <code>{b.releaseId}</code> 第 {b.index + 1} 批
                {b.capacityWaitSince && <>（{timeShort(b.capacityWaitSince)} 起）</>}：{b.capacityWait}
              </li>
            ))}
          </ul>
        </>
      )}

      {latest && (
        <>
          <h3><CalendarClock size={15} /> 当前可执行批次顺序
            <span className="hint">（{timeShort(latest.at)} 因“{latest.trigger}”重排）</span>
          </h3>
          {latest.entries.length === 0 && <p className="empty">当前没有待执行批次。</p>}
          {latest.entries.length > 0 && (
            <table>
              <thead><tr><th>顺序</th><th>发布</th><th>服务</th><th>批次</th><th>优先级</th><th>区域</th><th>预计开始</th><th>预计完成</th><th>说明</th></tr></thead>
              <tbody>
                {latest.entries.map((e) => (
                  <tr key={e.batchId}>
                    <td>{e.order}</td>
                    <td><code>{e.releaseId}</code></td>
                    <td>{e.serviceName}</td>
                    <td>第 {e.batchIndex + 1} 批</td>
                    <td>{e.priority}</td>
                    <td>{e.zones.join("、") || "—"}</td>
                    <td>{timeShort(e.estimatedStart)}</td>
                    <td>{timeShort(e.estimatedEnd)}</td>
                    <td>{e.note || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {latest.conflicts.length > 0 && (
            <div className="conflicts">
              <strong>容量冲突（{latest.conflicts.length}）：</strong>
              <ul>
                {latest.conflicts.map((c, i) => (
                  <li key={i} className={c.deadlineMiss ? "bad" : ""}>
                    {c.deadlineMiss ? "【无法按期完成】 " : ""}
                    <code>{c.releaseId}</code>（{c.serviceName}）第 {c.batchIndex + 1} 批：{c.reason}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </>
      )}

      {held.length > 0 && (
        <>
          <h3>占用明细</h3>
          <ul className="wait-list">
            {held.map((g) => (
              <li key={g.id}>
                <code>{g.releaseId}</code> 在 {snap.zones.find((z) => z.id === g.zoneId)?.name ?? g.zoneId}
                {" "}占用 {g.instanceIds.join("、")}（{timeShort(g.acquiredAt)} 取得）
              </li>
            ))}
          </ul>
        </>
      )}

      <div className="actions">
        <button onClick={() => setShowHistory(!showHistory)}>
          <History size={15} /> {showHistory ? "收起重排历史" : "重排历史（" + snap.plans.length + "）"}
        </button>
      </div>
      {showHistory && (
        <table>
          <thead><tr><th>时间</th><th>重排原因</th><th>纳入顺序</th><th>冲突</th></tr></thead>
          <tbody>
            {[...snap.plans].reverse().map((p) => (
              <tr key={p.id}>
                <td>{timeShort(p.at)}</td>
                <td>{p.trigger}</td>
                <td>{p.entries.length} 个批次</td>
                <td className={p.conflicts.length > 0 ? "bad" : ""}>{p.conflicts.length}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

