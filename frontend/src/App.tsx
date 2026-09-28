import { useEffect, useMemo, useState } from "react";
import { Activity, Ban, Boxes, GitBranch, ListOrdered, Play } from "lucide-react";
import { createRelease, cancelRelease, subscribe } from "./api";
import type { Batch, Snapshot } from "./types";

const phaseLabel: Record<string, string> = {
  pending: "等待",
  updating: "更新中",
  observing: "健康观察",
  succeeded: "成功",
  failed: "失败",
  cancelling: "取消中",
  rolling_back: "回退中",
  rolled_back: "已回退",
  cancelled: "已取消",
};

const releaseLabel: Record<string, string> = {
  pending: "等待开始",
  in_progress: "进行中",
  succeeded: "发布成功",
  failed: "失败",
  cancelling: "取消中",
  rolled_back: "失败已回退",
  cancelled: "已取消",
};

function timeShort(iso?: string): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleTimeString("zh-CN", { hour12: false });
}

export function App() {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [error, setError] = useState<string>("");
  const [target, setTarget] = useState("v2.0.0");
  const [batchSize, setBatchSize] = useState(2);
  const [mode, setMode] = useState("none");
  const [failInstances, setFailInstances] = useState("inst-1,inst-2");
  const [rollbackN, setRollbackN] = useState(2);

  useEffect(() => subscribe(setSnap), []);

  const activeRelease = useMemo(
    () => snap?.releases.find((r) => !["succeeded", "rolled_back", "cancelled", "failed"].includes(r.status)),
    [snap]
  );
  const latestRelease = snap?.releases[snap.releases.length - 1];

  if (!snap) return <div className="loading">正在加载演练状态…</div>;

  const submit = async () => {
    setError("");
    try {
      await createRelease({
        targetVersion: target,
        batchSize: Number(batchSize),
        strategy: "rolling",
        failure: {
          mode: mode as Snapshot["releases"][number]["failure"]["mode"],
          instances: mode === "instances" ? failInstances.split(",").map((s) => s.trim()).filter(Boolean) : undefined,
          failFirstNRollback: mode === "rollback_interrupt" ? Number(rollbackN) : undefined,
        },
      });
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const shownRelease = activeRelease ?? latestRelease;
  const batches = shownRelease ? snap.batches.filter((b) => b.releaseId === shownRelease.id) : [];

  return (
    <div className="page">
      <header>
        <h1><Boxes size={26} /> 软件发布演练平台</h1>
        <p className="sub">全部实例均为本地虚拟实例；更新、健康观察与回退均为模拟操作，不实际部署服务。</p>
      </header>

      <section className="grid">
        <div className="card">
          <h2><GitBranch size={18} /> 版本</h2>
          <table>
            <thead><tr><th>版本</th><th>名称</th><th>构建时间</th></tr></thead>
            <tbody>
              {snap.versions.map((v) => (
                <tr key={v.id} className={v.id === target ? "selected" : ""}>
                  <td><code>{v.id}</code></td><td>{v.name}</td><td>{timeShort(v.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <div className="card">
          <h2><Activity size={18} /> 虚拟实例（{snap.instances.length}）</h2>
          <table>
            <thead><tr><th>实例</th><th>当前版本</th><th>期望归宿</th><th>健康</th></tr></thead>
            <tbody>
              {snap.instances.map((i) => (
                <tr key={i.id}>
                  <td>{i.name}</td>
                  <td><code>{i.currentVersion}</code></td>
                  <td><code>{i.desiredVersion}</code></td>
                  <td><span className={"pill h-" + i.health}>{i.health === "healthy" ? "健康" : i.health === "unhealthy" ? "不健康" : "未知"}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <div className="card controls">
          <h2><Play size={18} /> 发起演练发布</h2>
          <label>目标版本
            <select value={target} onChange={(e) => setTarget(e.target.value)} disabled={!!activeRelease}>
              {snap.versions.map((v) => <option key={v.id} value={v.id}>{v.id}</option>)}
            </select>
          </label>
          <label>每批实例数
            <input type="number" min={1} max={snap.instances.length} value={batchSize}
              disabled={!!activeRelease} onChange={(e) => setBatchSize(Number(e.target.value))} />
          </label>
          <label>故障注入
            <select value={mode} onChange={(e) => setMode(e.target.value)} disabled={!!activeRelease}>
              <option value="none">无故障（正常发布）</option>
              <option value="half_batch">半批健康失败（首批前一半）</option>
              <option value="instances">指定实例健康失败</option>
              <option value="rollback_interrupt">半批失败 + 回退中断</option>
            </select>
          </label>
          {mode === "instances" && (
            <label>失败实例 ID（逗号分隔）
              <input value={failInstances} onChange={(e) => setFailInstances(e.target.value)} disabled={!!activeRelease} />
            </label>
          )}
          {mode === "rollback_interrupt" && (
            <label>每台实例回退中断次数
              <input type="number" min={1} value={rollbackN} disabled={!!activeRelease}
                onChange={(e) => setRollbackN(Number(e.target.value))} />
            </label>
          )}
          <div className="actions">
            <button className="primary" onClick={submit} disabled={!!activeRelease}>开始按批次发布</button>
            {activeRelease && (
              <button className="danger" onClick={() => cancelRelease(activeRelease.id)}>
                <Ban size={15} /> 取消发布
              </button>
            )}
          </div>
          {error && <p className="error">{error}</p>}
          <p className="hint">取消后不会再扩到后续批次；当前批次中已更新实例会明确回退到原版本。</p>
        </div>
      </section>

      {shownRelease && (
        <section className="card release">
          <h2><ListOrdered size={18} /> 发布 {shownRelease.id}
            <span className={"pill r-" + shownRelease.status}>{releaseLabel[shownRelease.status] ?? shownRelease.status}</span>
          </h2>
          <p className="sub">
            目标 <code>{shownRelease.targetVersion}</code>，基线 <code>{shownRelease.baselineVersion}</code>，
            策略 {shownRelease.strategy}，每批 {shownRelease.batchSize} 台 · {shownRelease.message ?? "进行中，所有进度实时持久化"}
          </p>
          <div className="batches">
            {batches.map((b) => <BatchView key={b.id} batch={b} ops={snap.ops} />)}
          </div>
        </section>
      )}

      <section className="card events">
        <h2>操作过程（事件流）</h2>
        <div className="event-list">
          {[...snap.events].reverse().slice(0, 60).map((e) => (
            <div key={e.seq} className={"event lv-" + e.level}>
              <span className="t">{timeShort(e.at)}</span>
              <span className="m">{e.message}</span>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

function BatchView({ batch, ops }: { batch: Batch; ops: Snapshot["ops"] }) {
  const pendingOps = ops.filter((o) => o.batchId === batch.id);
  return (
    <div className={"batch ph-" + batch.phase}>
      <div className="batch-head">
        <strong>第 {batch.index + 1} 批</strong>
        <span className={"pill b-" + batch.phase}>{phaseLabel[batch.phase] ?? batch.phase}</span>
        <span className="when">{timeShort(batch.startedAt)} → {timeShort(batch.finishedAt)}</span>
      </div>
      <div className="members">
        {batch.members.map((m) => (
          <div key={m.instanceId} className="member">
            <div className="m-title">
              <strong>{m.instanceId}</strong>
              <code>{m.fromVersion} → {m.toVersion}</code>
            </div>
            <div className="m-states">
              <span className={"pill u-" + m.updateState}>{phaseMember(m.updateState)}</span>
              <span className={"pill hh-" + m.healthState}>{healthLabel(m.healthState)}</span>
            </div>
            <p className="m-msg">{m.message || "等待处理"}</p>
          </div>
        ))}
      </div>
      {pendingOps.length > 0 && (
        <div className="ops">排队中的虚拟操作：
          {pendingOps.map((o) => (
            <span key={o.id} className="op-tag">
              {o.kind === "update" ? "更新" : o.kind === "health" ? "健康观察" : "回退"}
              {o.attempt > 0 ? "·重试" + o.attempt : ""}（{o.instanceId}）
            </span>
          ))}
        </div>
      )}
    </div>
  );
}

function phaseMember(s: string): string {
  const m: Record<string, string> = {
    pending: "未更新", in_progress: "更新中", succeeded: "已更新", failed: "更新失败",
    rollback_in_progress: "回退中", rolled_back: "已回退",
  };
  return m[s] ?? s;
}

function healthLabel(s: string): string {
  const m: Record<string, string> = {
    pending: "未观察", in_progress: "观察中", healthy: "健康", unhealthy: "不健康", stale: "迟到作废",
  };
  return m[s] ?? s;
}

