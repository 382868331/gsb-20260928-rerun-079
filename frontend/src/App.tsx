import { Component, useEffect, useMemo, useState, type ReactNode } from "react";
import { Activity, Ban, Boxes, FileClock, GitBranch, ListOrdered, OctagonAlert, Pause, Play, RotateCcw, Shrink, SkipForward } from "lucide-react";
import {
  createRelease, cancelRelease, finishDrill, subscribe, initEnvironment,
  pauseForReview, reviewContinue, reviewShrink, reviewRollback,
} from "./api";
import type { Batch, Snapshot } from "./types";

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

// ErrorBoundary keeps a single bad render from producing a permanent white
// screen; it shows the underlying failure and a reload action instead.
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    if (this.state.error) {
      return (
        <div className="page">
          <div className="card fatal">
            <h2><OctagonAlert size={18} /> 页面渲染失败</h2>
            <p className="error">{String(this.state.error.message || this.state.error)}</p>
            <button className="primary" onClick={() => window.location.reload()}>重新加载</button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}

export function App() {
  const [snap, setSnap] = useState<Snapshot | null>(null);
  const [error, setError] = useState<string>("");
  const [initializing, setInitializing] = useState(false);
  const [serviceId, setServiceId] = useState("order");
  const [target, setTarget] = useState("v2.0.0");
  const [batchSize, setBatchSize] = useState(2);
  const [mode, setMode] = useState("none");
  const [failInstances, setFailInstances] = useState("inst-1,inst-2");
  const [rollbackN, setRollbackN] = useState(2);
  const [strategy, setStrategy] = useState("rolling");
  const [pauseOnFailure, setPauseOnFailure] = useState(true);

  useEffect(
    () =>
      subscribe((s) => {
        setSnap(s);
        setError("");
      }),
    []
  );

  const terminal = ["succeeded", "rolled_back", "cancelled", "failed"];
  const activeReleases = useMemo(
    () => (snap?.releases ?? []).filter((r) => !terminal.includes(r.status)),
    [snap]
  );

  if (!snap) return <div className="loading">正在加载演练状态…</div>;

  const envEmpty = snap.versions.length === 0 && snap.instances.length === 0;
  const serviceName = (id: string) => snap.services.find((s) => s.id === id)?.name ?? id;
  const serviceVersions = snap.versions.filter((v) => v.serviceId === serviceId);
  const activeForService = activeReleases.find((r) => r.serviceId === serviceId);
  const effectiveTarget = serviceVersions.some((v) => v.id === target)
    ? target
    : serviceVersions[serviceVersions.length - 1]?.id ?? "";

  const initEnv = async () => {
    setError("");
    setInitializing(true);
    try {
      setSnap(await initEnvironment());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setInitializing(false);
    }
  };

  const submit = async () => {
    setError("");
    try {
      await createRelease({
        targetVersion: effectiveTarget,
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

  const latestTerminal = [...snap.releases].reverse().find((r) => terminal.includes(r.status));
  const shownReleases = [...activeReleases];
  if (latestTerminal && !shownReleases.some((r) => r.id === latestTerminal.id)) {
    shownReleases.push(latestTerminal);
  }

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
            <thead><tr><th>服务</th><th>版本</th><th>名称</th><th>构建时间</th></tr></thead>
            <tbody>
              {snap.versions.map((v) => (
                <tr key={v.id} className={v.id === effectiveTarget ? "selected" : ""}>
                  <td>{serviceName(v.serviceId)}</td>
                  <td><code>{v.id}</code></td><td>{v.name}</td><td>{timeShort(v.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {snap.versions.length === 0 && (
            <p className="empty">暂无版本：点击下方“初始化演练环境”创建基线与目标版本。</p>
          )}
        </div>

        <div className="card">
          <h2><Activity size={18} /> 虚拟实例（{snap.instances.length}）</h2>
          <table>
            <thead><tr><th>服务</th><th>实例</th><th>当前版本</th><th>期望归宿</th><th>健康</th></tr></thead>
            <tbody>
              {snap.instances.map((i) => (
                <tr key={i.id}>
                  <td>{serviceName(i.serviceId)}</td>
                  <td>{i.name}</td>
                  <td><code>{i.currentVersion}</code></td>
                  <td><code>{i.desiredVersion}</code></td>
                  <td><span className={"pill h-" + i.health}>{i.health === "healthy" ? "健康" : i.health === "unhealthy" ? "不健康" : "未知"}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
          {snap.instances.length === 0 && (
            <p className="empty">暂无虚拟实例：初始化后将创建 3 个服务共 10 台运行基线版本的实例。</p>
          )}
        </div>

        <div className="card controls">
          <h2><Play size={18} /> 发起演练发布</h2>
          <label>服务
            <select value={serviceId} onChange={(e) => setServiceId(e.target.value)} disabled={!!activeForService}>
              {snap.services.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
          </label>
          <label>目标版本
            <select value={effectiveTarget} onChange={(e) => setTarget(e.target.value)} disabled={!!activeForService}>
              {serviceVersions.map((v) => <option key={v.id} value={v.id}>{v.id}</option>)}
            </select>
          </label>
          <label>发布策略
            <select value={strategy} onChange={(e) => setStrategy(e.target.value)} disabled={!!activeForService}>
              <option value="rolling">分批滚动（按批次逐步扩批）</option>
              <option value="all_at_once">全量一次性（全部实例同批）</option>
            </select>
          </label>
          <label>每批实例数
            <input type="number" min={1} max={snap.instances.filter((i) => i.serviceId === serviceId).length} value={batchSize}
              disabled={!!activeForService || strategy === "all_at_once"} onChange={(e) => setBatchSize(Number(e.target.value))} />
          </label>
          <label>故障注入
            <select value={mode} onChange={(e) => setMode(e.target.value)} disabled={!!activeForService}>
              <option value="none">无故障（正常发布）</option>
              <option value="half_batch">半批健康失败（首批前一半）</option>
              <option value="instances">指定实例健康失败</option>
              <option value="rollback_interrupt">半批失败 + 回退中断</option>
            </select>
          </label>
          {mode === "instances" && (
            <label>失败实例 ID（逗号分隔）
              <input value={failInstances} onChange={(e) => setFailInstances(e.target.value)} disabled={!!activeForService} />
            </label>
          )}
          {mode === "rollback_interrupt" && (
            <label>每台实例回退中断次数
              <input type="number" min={1} value={rollbackN} disabled={!!activeForService}
                onChange={(e) => setRollbackN(Number(e.target.value))} />
            </label>
          )}
          <label className="check">
            <input type="checkbox" checked={pauseOnFailure} disabled={!!activeForService}
              onChange={(e) => setPauseOnFailure(e.target.checked)} />
            失败后暂停，审查失败实例再决定继续回退或结束
          </label>
          <div className="actions">
            <button className="primary" onClick={submit} disabled={!!activeForService || envEmpty || !effectiveTarget}>开始按批次发布</button>
            {activeForService && !activeForService.pauseRequested && activeForService.status !== "paused" && (
              <button onClick={() => pauseForReview(activeForService.id).catch((e) => setError(e.message))}>
                <Pause size={15} /> 暂停审查
              </button>
            )}
            {activeForService && (
              <button className="danger" onClick={() => cancelRelease(activeForService.id).catch((e) => setError(e.message))}>
                <Ban size={15} /> 取消发布
              </button>
            )}
          </div>
          {error && <p className="error">{error}</p>}
          {envEmpty && (
            <div className="init-box">
              <p className="hint">当前是完全空的演练环境（无版本、无实例、无发布记录）。先初始化环境，再创建演练发布；发布记录只会在你点击“开始”后生成。</p>
              <button className="primary" onClick={initEnv} disabled={initializing}>
                {initializing ? "正在初始化…" : "初始化演练环境（3 个服务 + 10 台实例）"}
              </button>
            </div>
          )}
          <p className="hint">取消后不会再扩到后续批次；当前批次中已更新实例会明确回退到原版本。每个批次生效前都会先检查服务间的版本兼容约束与相关服务的健康依据。</p>
        </div>
      </section>

      {snap.services.length > 0 && (
        <section className="card">
          <h2><Boxes size={18} /> 服务与版本兼容约束</h2>
          <div className="services">
            {snap.services.map((s) => {
              const insts = snap.instances.filter((i) => i.serviceId === s.id);
              const vers = [...new Set(insts.map((i) => i.currentVersion))];
              return (
                <div key={s.id} className="service-box">
                  <strong>{s.name}</strong>
                  <span className="hint">{insts.length} 台实例 · 当前 {vers.map((v) => <code key={v}>{v}</code>)}</span>
                </div>
              );
            })}
          </div>
          <ul className="rule-list">
            {snap.compatRules.map((r) => (
              <li key={r.id}>
                <span className={"pill rule-" + r.mode}>{ruleModeLabel(r.mode)}</span>
                {r.note}
              </li>
            ))}
          </ul>
        </section>
      )}

      <VersionDistribution snap={snap} batches={snap.batches.filter((b) => shownReleases.some((r) => r.id === b.releaseId))} />

      {shownReleases.length > 0 ? (
        shownReleases.map((rel) => (
          <ReleaseSection key={rel.id} snap={snap} release={rel} serviceName={serviceName} onError={setError} />
        ))
      ) : (
        <section className="card release empty-release">
          <h2><ListOrdered size={18} /> 发布状态</h2>
          <p className="empty">
            {envEmpty
              ? "环境尚未初始化，暂无发布记录。初始化环境并发起演练后，这里会实时展示批次、实例更新与健康证据。"
              : "暂无发布记录。在上方选择目标版本、策略与故障注入后点击“开始按批次发布”，所有进度都会持久化，刷新或重开页面不丢失。"}
          </p>
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

function ruleModeLabel(mode: string): string {
  const m: Record<string, string> = {
    dependent_first: "依赖者先更新",
    stay_old: "保持旧版",
    pause: "暂停推进",
  };
  return m[mode] ?? mode;
}

// ReleaseSection renders one release (active or the latest finished one)
// with its batches, compat evidence and rollback impact.
function ReleaseSection({
  snap,
  release,
  serviceName,
  onError,
}: {
  snap: Snapshot;
  release: Snapshot["releases"][number];
  serviceName: (id: string) => string;
  onError: (m: string) => void;
}) {
  const batches = snap.batches.filter((b) => b.releaseId === release.id);
  return (
    <section className="card release">
      <h2><ListOrdered size={18} /> 发布 {release.id}（{serviceName(release.serviceId)}）
        <span className={"pill r-" + release.status}>{releaseLabel[release.status] ?? release.status}</span>
      </h2>
      <p className="sub">
        目标 <code>{release.targetVersion}</code>，基线 <code>{release.baseline}</code>，
        策略 {release.strategy}，每批 {release.batchSize} 台 · {release.message ?? "进行中，所有进度实时持久化"}
      </p>
      {release.status === "paused" && (
        <PausedReview snap={snap} release={release} onError={onError} />
      )}
      {(release.rollbackImpact?.length ?? 0) > 0 && (
        <div className="impact">
          <h3><RotateCcw size={15} /> 回退影响说明</h3>
          <ul>
            {release.rollbackImpact!.map((line, i) => <li key={i}>{line}</li>)}
          </ul>
        </div>
      )}
      <div className="batches">
        {batches.map((b) => <BatchView key={b.id} batch={b} ops={snap.ops} serviceName={serviceName} />)}
      </div>
    </section>
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
  const decided = !!release.reviewAction;
  const [shrinkSize, setShrinkSize] = useState(1);
  const actionLabel: Record<string, string> = {
    continue: "继续（按原批次）",
    shrink: "缩小后续批次",
    rollback: "回退当前批次",
    finish: "结束演练（保留现状）",
    cancel: "取消发布",
  };
  return (
    <div className="review">
      <h3><OctagonAlert size={16} /> 暂停审查：第 {pausedBatch ? pausedBatch.index + 1 : "-"} 批出现失败实例</h3>
      <p className="sub">实例状态来自持久化的真实运行记录，刷新或重新打开页面仍停在该状态，审查后再决定走向。</p>
      <p className="hint">暂停后到达的健康结果仍会作为证据显示在每个实例上，但不会改变已经做出的终止决定。</p>
      {decided && (
        <p className="decision">审查决定已确定并持久化：{actionLabel[release.reviewAction ?? ""] ?? release.reviewAction}
          {release.reviewAction === "shrink" && release.reviewShrinkTo ? `（后续每批 ${release.reviewShrinkTo} 台）` : ""}
          ，决定时间 {timeShort(release.reviewAt)}。重复操作不会再次执行已完成的批次。
        </p>
      )}
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
      {!decided && (
        <div className="review-actions">
          <div className="actions">
            <button className="primary" onClick={() => reviewContinue(release.id).catch((e) => onError(e.message))}>
              <Play size={15} /> 证据通过，继续发布
            </button>
            <button
              className="warning"
              disabled={shrinkSize >= release.batchSize}
              onClick={() => reviewShrink(release.id, Number(shrinkSize)).catch((e) => onError(e.message))}
            >
              <Shrink size={15} /> 缩小批次后继续
            </button>
            <button onClick={() => reviewRollback(release.id).catch((e) => onError(e.message))}>
              <RotateCcw size={15} /> 回退本批
            </button>
            <button className="danger" onClick={() => finishDrill(release.id).catch((e) => onError(e.message))}>
              <SkipForward size={15} /> 结束演练（保留当前版本分布）
            </button>
          </div>
          <label className="shrink-size">缩小后每批实例数
            <input
              type="number"
              min={1}
              max={Math.max(1, release.batchSize - 1)}
              value={shrinkSize}
              onChange={(e) => setShrinkSize(Number(e.target.value))}
            />
            <span className="hint">（当前每批 {release.batchSize} 台；仅影响尚未开始的批次，当前批次不重跑）</span>
          </label>
        </div>
      )}
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

function BatchView({ batch, ops, serviceName }: { batch: Batch; ops: Snapshot["ops"]; serviceName: (id: string) => string }) {
  const pendingOps = ops.filter((o) => o.batchId === batch.id);
  return (
    <div className={"batch ph-" + batch.phase}>
      <div className="batch-head">
        <strong>第 {batch.index + 1} 批</strong>
        <span className={"pill b-" + batch.phase}>{phaseLabel[batch.phase] ?? batch.phase}</span>
        <span className="when">{timeShort(batch.startedAt)} → {timeShort(batch.finishedAt)}</span>
      </div>
      {batch.blockedReason && (
        <p className="blocked"><OctagonAlert size={14} /> {batch.blockedReason}（约束满足后自动继续，已完成的操作不会丢失）</p>
      )}
      {(batch.compat?.length ?? 0) > 0 && (
        <ul className="compat-list">
          {batch.compat!.map((c, i) => (
            <li key={i} className={c.ok ? "ok" : "bad"}>
              {c.ok ? "✓" : "✗"} [{c.kind === "health" ? "健康" : "兼容"}·{serviceName(c.serviceId)}] {c.detail}
            </li>
          ))}
        </ul>
      )}
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
            {m.lateHealthResult && (
              <p className="late">
                <FileClock size={13} /> 迟到健康证据：{m.lateHealthVersion} 判定为
                <span className={"pill hh-" + (m.lateHealthResult === "healthy" ? "healthy" : "unhealthy")}>
                  {m.lateHealthResult === "healthy" ? "健康" : "不健康"}
                </span>
                （{timeShort(m.lateHealthAt)} 到达，仅留档，不改变决定）
              </p>
            )}
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
