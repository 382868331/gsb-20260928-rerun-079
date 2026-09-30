import { useEffect, useMemo, useState } from "react";
import { Activity, Ban, Boxes, GitBranch, ListOrdered, OctagonAlert, PauseCircle, Play, RotateCcw, Shrink, SkipForward } from "lucide-react";
import { createRelease, cancelRelease, resumeRollback, finishDrill, pauseRelease, continueRelease, shrinkRelease, rollbackRelease, subscribe } from "./api";
import type { Batch, Release, Snapshot } from "./types";

const phaseLabel: Record<string, string> = {
  pending: "等待",
  updating: "更新中",
  observing: "健康观察",
  succeeded: "成功",
  failed: "失败",
  cancelling: "取消中",
  paused: "暂停待审查",
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
  paused: "暂停待审查",
  rolling_back: "回退中",
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
  const [loadError, setLoadError] = useState<string>("");
  const [target, setTarget] = useState("v2.0.0");
  const [batchSize, setBatchSize] = useState(2);
  const [mode, setMode] = useState("none");
  const [failInstances, setFailInstances] = useState("inst-1,inst-2");
  const [rollbackN, setRollbackN] = useState(2);
  const [strategy, setStrategy] = useState("rolling");
  const [pauseOnFailure, setPauseOnFailure] = useState(true);

  useEffect(
    () =>
      subscribe(
        (s) => {
          setSnap(s);
          setLoadError("");
        },
        (m) => setLoadError(m)
      ),
    []
  );

  const activeRelease = useMemo(
    () => (snap?.releases ?? []).find((r) => !["succeeded", "rolled_back", "cancelled", "failed"].includes(r.status)),
    [snap]
  );
  const releases = snap?.releases ?? [];
  const latestRelease = releases[releases.length - 1];

  if (!snap) {
    if (loadError) {
      return (
        <div className="loading">
          <div className="load-failure">
            <h2>无法连接演练平台后端</h2>
            <p>{loadError}</p>
            <p className="hint">这不是空数据：后端未启动或接口异常时不会展示任何模拟状态。</p>
            <button className="primary" onClick={() => window.location.reload()}>重试</button>
          </div>
        </div>
      );
    }
    return <div className="loading">正在加载演练状态…</div>;
  }

  const submit = async () => {
    setError("");
    try {
      await createRelease({
        targetVersion: target,
        batchSize: Number(batchSize),
        strategy: strategy as "rolling" | "all_at_once",
        failure: {
          mode: mode as Snapshot["releases"][number]["failure"]["mode"],
          instances: mode === "instances" ? failInstances.split(",").map((s) => s.trim()).filter(Boolean) : undefined,
          failFirstNRollback: mode === "rollback_interrupt" ? Number(rollbackN) : undefined,
          pauseOnFailure,
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
              {snap.versions.length === 0 && (
                <tr><td colSpan={3} className="empty-cell">暂无版本记录</td></tr>
              )}
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
              {snap.instances.length === 0 && (
                <tr><td colSpan={4} className="empty-cell">暂无虚拟实例</td></tr>
              )}
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
          <label>发布策略
            <select value={strategy} onChange={(e) => setStrategy(e.target.value)} disabled={!!activeRelease}>
              <option value="rolling">分批滚动（按批次逐步扩批）</option>
              <option value="all_at_once">全量一次性（全部实例同批）</option>
            </select>
          </label>
          <label>每批实例数
            <input type="number" min={1} max={snap.instances.length} value={batchSize}
              disabled={!!activeRelease || strategy === "all_at_once"} onChange={(e) => setBatchSize(Number(e.target.value))} />
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
          <label className="check">
            <input type="checkbox" checked={pauseOnFailure} disabled={!!activeRelease}
              onChange={(e) => setPauseOnFailure(e.target.checked)} />
            失败后暂停，审查失败实例再决定继续回退或结束
          </label>
          <div className="actions">
            <button className="primary" onClick={submit} disabled={!!activeRelease || snap.versions.length === 0 || snap.instances.length === 0}>
              开始按批次发布
            </button>
            {activeRelease && activeRelease.status === "in_progress" && !activeRelease.reviewRequested && (
              <button onClick={() => pauseRelease(activeRelease.id).catch((e) => setError(e.message))}>
                <PauseCircle size={15} /> 本批完成后暂停审查
              </button>
            )}
            {activeRelease && (
              <button className="danger" onClick={() => cancelRelease(activeRelease.id).catch((e) => setError(e.message))}>
                <Ban size={15} /> 取消发布
              </button>
            )}
          </div>
          {activeRelease?.reviewRequested && activeRelease.status !== "paused" && (
            <p className="hint">已请求审查暂停：当前批次健康证据收集完成后将自动暂停。</p>
          )}
          {error && <p className="error">{error}</p>}
          <p className="hint">取消后不会再扩到后续批次；当前批次中已更新实例会明确回退到原版本。</p>
        </div>
      </section>

      {snap.instances.length > 0 && <VersionDistribution snap={snap} batches={shownRelease ? batches : []} />}

      {!shownRelease && (
        <section className="card empty-state">
          <h2><ListOrdered size={18} /> 发布记录</h2>
          <p className="sub">暂无发布记录。这是空状态而非加载失败：在上方选择目标版本与策略，创建第一个演练发布。</p>
        </section>
      )}

      {shownRelease && (
        <section className="card release">
          <h2><ListOrdered size={18} /> 发布 {shownRelease.id}
            <span className={"pill r-" + shownRelease.status}>{releaseLabel[shownRelease.status] ?? shownRelease.status}</span>
          </h2>
          <p className="sub">
            目标 <code>{shownRelease.targetVersion}</code>，基线 <code>{shownRelease.baseline}</code>，
            策略 {shownRelease.strategy}，每批 {shownRelease.batchSize} 台 · {shownRelease.message ?? "进行中，所有进度实时持久化"}
          </p>
          {shownRelease.status === "paused" && shownRelease.pauseKind === "review" && (
            <HealthReview snap={snap} release={shownRelease} onError={setError} />
          )}
          {shownRelease.status === "paused" && shownRelease.pauseKind !== "review" && (
            <PausedReview snap={snap} release={shownRelease} onError={setError} />
          )}
          <div className="batches">
            {batches.map((b) => <BatchView key={b.id} batch={b} ops={snap.ops} />)}
          </div>
        </section>
      )}

      <section className="card events">
        <h2>操作过程（事件流）</h2>
        <div className="event-list">
          {snap.events.length === 0 && <div className="event lv-info"><span className="m">暂无事件</span></div>}
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

function PausedReview({
  snap,
  release,
  onError,
}: {
  snap: Snapshot;
  release: Snapshot["releases"][number];
  onError: (m: string) => void;
}) {
  const pausedBatch = snap.batches
    .filter((b) => b.releaseId === release.id && b.phase === "paused")
    .sort((a, b) => a.index - b.index)[0];
  const members = pausedBatch?.members ?? [];
  const failed = members.filter((m) => m.updateState === "failed" || m.healthState === "unhealthy");
  const onNew = members.filter((m) => m.updateState === "succeeded" && m.healthState !== "unhealthy");
  const stillOld = members.filter((m) => m.updateState !== "succeeded");
  return (
    <div className="review">
      <h3><OctagonAlert size={16} /> 暂停审查：第 {pausedBatch ? pausedBatch.index + 1 : "-"} 批出现失败实例</h3>
      <p className="sub">实例状态来自持久化的真实运行记录，刷新或重新打开页面仍停在该状态，审查后再决定走向。</p>
      <ul className="review-list">
        {failed.map((m) => (
          <li key={m.instanceId} className="bad">
            <code>{m.instanceId}</code> 失败：{m.message || "更新或健康检查失败"}
          </li>
        ))}
        {onNew.map((m) => (
          <li key={m.instanceId} className="new">
            <code>{m.instanceId}</code> 已在新版本 <code>{m.toVersion}</code>，回退会返回 <code>{m.fromVersion}</code>
          </li>
        ))}
        {stillOld.map((m) => (
          <li key={m.instanceId} className="old">
            <code>{m.instanceId}</code> 未离开原版本 <code>{m.fromVersion}</code>
          </li>
        ))}
      </ul>
      <div className="actions">
        <button className="primary" onClick={() => resumeRollback(release.id).catch((e) => onError(e.message))}>
          <RotateCcw size={15} /> 继续回退本批
        </button>
        <button className="danger" onClick={() => finishDrill(release.id).catch((e) => onError(e.message))}>
          <SkipForward size={15} /> 结束本次演练（保留当前版本分布）
        </button>
      </div>
    </div>
  );
}

// HealthReview is shown when a release is paused for manual review after a
// batch's health evidence completed. The operator inspects the persisted
// evidence, then chooses to continue, shrink the remaining batches, or roll
// back everything that moved.
function HealthReview({
  snap,
  release,
  onError,
}: {
  snap: Snapshot;
  release: Release;
  onError: (m: string) => void;
}) {
  const [shrinkTo, setShrinkTo] = useState(1);
  const relBatches = snap.batches.filter((b) => b.releaseId === release.id).sort((a, b) => a.index - b.index);
  const pausedBatch = relBatches.find((b) => b.phase === "paused");
  const doneBatches = relBatches.filter((b) => b.phase === "succeeded");
  const pendingBatches = relBatches.filter((b) => b.phase === "pending");
  const remaining = pendingBatches.reduce((n, b) => n + b.members.length, 0);
  const updatedCount = snap.instances.filter((i) => i.currentVersion === release.targetVersion).length;
  const evidence = pausedBatch?.members ?? [];
  return (
    <div className="review">
      <h3><PauseCircle size={16} /> 人工审查：第 {pausedBatch ? pausedBatch.index + 1 : "-"} 批健康证据已就绪</h3>
      <p className="sub">
        以下健康证据来自持久化的真实运行记录；刷新或重开页面后审查状态与选择保持不变。
        已完成 {doneBatches.length + (pausedBatch ? 1 : 0)} 批，{updatedCount} 台实例已在新版本，剩余 {remaining} 台未发布。
      </p>
      <ul className="review-list">
        {evidence.map((m) => (
          <li key={m.instanceId} className={m.healthState === "healthy" ? "new" : "bad"}>
            <code>{m.instanceId}</code> {m.fromVersion} → {m.toVersion}：
            {m.healthState === "healthy" ? "健康检查通过" : m.message || "健康证据异常"}
            {m.message && m.healthState === "healthy" ? "（" + m.message + "）" : ""}
          </li>
        ))}
      </ul>
      <div className="actions">
        <button className="primary" onClick={() => continueRelease(release.id).catch((e) => onError(e.message))}>
          <Play size={15} /> 继续发布下一批
        </button>
        {remaining > 0 && (
          <span className="shrink-box">
            <input
              type="number" min={1} max={remaining} value={shrinkTo}
              onChange={(e) => setShrinkTo(Number(e.target.value))}
            />
            <button onClick={() => shrinkRelease(release.id, shrinkTo).catch((e) => onError(e.message))}>
              <Shrink size={15} /> 缩小批次并继续
            </button>
          </span>
        )}
        <button className="danger" onClick={() => rollbackRelease(release.id).catch((e) => onError(e.message))}>
          <RotateCcw size={15} /> 回退全部已更新实例
        </button>
      </div>
    </div>
  );
}

// VersionDistribution counts versions across real instance records. Instances
// still marked rollback_in_progress are reported separately so a rolling-back
// instance is never counted as a finished rollback (or as a successful new
// version) on a fake progress bar.
function VersionDistribution({ snap, batches }: { snap: Snapshot; batches: Batch[] }) {
  const memberByInstance = new Map<string, Snapshot["batches"][number]["members"][number]>();
  for (const b of batches) {
    for (const m of b.members) memberByInstance.set(m.instanceId, m);
  }
  const counts = new Map<string, number>();
  let rolling = 0;
  for (const inst of snap.instances) {
    const m = memberByInstance.get(inst.id);
    if (m?.updateState === "rollback_in_progress") {
      rolling++;
      continue;
    }
    counts.set(inst.currentVersion, (counts.get(inst.currentVersion) ?? 0) + 1);
  }
  const total = snap.instances.length;
  return (
    <section className="card">
      <h2><GitBranch size={18} /> 真实版本分布（按实例当前版本统计，共 {total} 台）</h2>
      <div className="dist">
        {[...counts.entries()].map(([v, n]) => (
          <div key={v} className="dist-row">
            <code>{v}</code>
            <div className="bar"><div className="fill" style={{ width: `${(n / total) * 100}%` }} /></div>
            <span className="num">{n} 台</span>
          </div>
        ))}
        {rolling > 0 && (
          <div className="dist-row">
            <code className="muted">回退中（归宿未定）</code>
            <div className="bar"><div className="fill rolling-fill" style={{ width: `${(rolling / total) * 100}%` }} /></div>
            <span className="num">{rolling} 台</span>
          </div>
        )}
      </div>
    </section>
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
