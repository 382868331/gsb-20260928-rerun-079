import { Component, useEffect, useMemo, useState, type ReactNode } from "react";
import { Activity, Ban, Boxes, FileClock, GitBranch, Link2, ListOrdered, OctagonAlert, Pause, Play, Plus, RotateCcw, Shrink, SkipForward } from "lucide-react";
import {
  addInstance, addRule, createRelease, cancelRelease, finishDrill, subscribe, initEnvironment,
  pauseForReview, reviewContinue, reviewShrink, reviewRollback, setCapacityPolicy,
} from "./api";
import type { Batch, Snapshot, Zone } from "./types";

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
            <p className="hint">已完成的操作都已持久化在服务端，不会丢失。可以先重试渲染，或重新加载页面。</p>
            <div className="actions">
              <button className="primary" onClick={() => this.setState({ error: null })}>重试</button>
              <button onClick={() => window.location.reload()}>重新加载</button>
            </div>
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
  const [target, setTarget] = useState("v2.0.0");
  const [batchSize, setBatchSize] = useState(2);
  const [mode, setMode] = useState("none");
  const [failInstances, setFailInstances] = useState("inst-1,inst-2");
  const [rollbackN, setRollbackN] = useState(2);
  const [strategy, setStrategy] = useState("rolling");
  const [pauseOnFailure, setPauseOnFailure] = useState(true);
  const [serviceId, setServiceId] = useState("");

  useEffect(
    () =>
      subscribe((s) => {
        setSnap(s);
        setError("");
      }),
    []
  );

  const activeRelease = useMemo(
    () => snap?.releases.find((r) => !["succeeded", "rolled_back", "cancelled", "failed"].includes(r.status)),
    [snap]
  );
  const latestRelease = snap?.releases[snap.releases.length - 1];

  if (!snap) return <div className="loading">正在加载演练状态…</div>;

  const envEmpty = snap.versions.length === 0 && snap.instances.length === 0;

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
        serviceId: serviceId || undefined,
        targetVersion: target,
        batchSize: Number(batchSize),
        strategy: strategy as "rolling" | "all_at_once" | "zone_spread",
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
  const serviceName = (id: string) => snap.services.find((s) => s.id === id)?.name ?? id;
  const releaseDeps = shownRelease
    ? snap.dependencies.find((d) => d.releaseId === shownRelease.id)
    : undefined;

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
          {snap.versions.length === 0 && (
            <p className="empty">暂无版本：点击下方“初始化演练环境”创建基线与目标版本。</p>
          )}
        </div>

        <div className="card">
          <h2><Activity size={18} /> 虚拟实例（{snap.instances.length}）</h2>
          <table>
            <thead><tr><th>实例</th><th>区域</th><th>当前版本</th><th>期望归宿</th><th>健康</th></tr></thead>
            <tbody>
              {snap.instances.map((i) => (
                <tr key={i.id}>
                  <td>{i.name}</td>
                  <td>{snap.zones.find((z) => z.id === i.zoneId)?.name ?? i.zoneId ?? "—"}</td>
                  <td><code>{i.currentVersion}</code></td>
                  <td><code>{i.desiredVersion}</code></td>
                  <td><span className={"pill h-" + i.health}>{i.health === "healthy" ? "健康" : i.health === "unhealthy" ? "不健康" : "未知"}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
          {snap.instances.length === 0 && (
            <p className="empty">暂无虚拟实例：初始化后将创建 6 台运行基线版本的实例。</p>
          )}
        </div>

        <div className="card controls">
          <h2><Play size={18} /> 发起演练发布</h2>
          <label>目标服务
            <select value={serviceId || snap.services[0]?.id || ""} onChange={(e) => setServiceId(e.target.value)} disabled={!!activeRelease}>
              {snap.services.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
          </label>
          <label>目标版本
            <select value={target} onChange={(e) => setTarget(e.target.value)} disabled={!!activeRelease}>
              {snap.versions.map((v) => <option key={v.id} value={v.id}>{v.id}</option>)}
            </select>
          </label>
          <label>发布策略
            <select value={strategy} onChange={(e) => setStrategy(e.target.value)} disabled={!!activeRelease}>
              <option value="rolling">分批滚动（按批次逐步扩批）</option>
              <option value="zone_spread">跨区域分散（每批只含同一区域，区域间交替推进）</option>
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
            <button className="primary" onClick={submit} disabled={!!activeRelease || envEmpty}>开始按批次发布</button>
            {activeRelease && !activeRelease.pauseRequested && activeRelease.status !== "paused" && (
              <button onClick={() => pauseForReview(activeRelease.id).catch((e) => setError(e.message))}>
                <Pause size={15} /> 暂停审查
              </button>
            )}
            {activeRelease && (
              <button className="danger" onClick={() => cancelRelease(activeRelease.id).catch((e) => setError(e.message))}>
                <Ban size={15} /> 取消发布
              </button>
            )}
          </div>
          {error && <p className="error">{error}</p>}
          {envEmpty && (
            <div className="init-box">
              <p className="hint">当前是完全空的演练环境（无版本、无服务、无实例、无发布记录）。先初始化环境，再创建演练发布；发布记录只会在你点击“开始”后生成。</p>
              <button className="primary" onClick={initEnv} disabled={initializing}>
                {initializing ? "正在初始化…" : "初始化演练环境（3 个服务 + 2 个区域 + 2 个版本 + 6 台实例 + 4 条兼容约束）"}
              </button>
            </div>
          )}
          <p className="hint">取消后不会再扩到后续批次；当前批次中已更新实例会明确回退到原版本。</p>
        </div>
      </section>

      {!envEmpty && (
        <section className="grid">
          <ServicesCard snap={snap} onError={setError} />
          <RulesCard snap={snap} onError={setError} />
        </section>
      )}

      {!envEmpty && (
        <section className="grid">
          <ZonesCard snap={snap} onError={setError} />
          <ReservationsCard snap={snap} />
        </section>
      )}

      <VersionDistribution snap={snap} batches={shownRelease ? batches : []} />

      {shownRelease ? (
        <section className="card release">
          <h2><ListOrdered size={18} /> 发布 {shownRelease.id}
            <span className={"pill r-" + shownRelease.status}>{releaseLabel[shownRelease.status] ?? shownRelease.status}</span>
          </h2>
          <p className="sub">
            服务 {serviceName(shownRelease.serviceId)} · 目标 <code>{shownRelease.targetVersion}</code>，基线 <code>{shownRelease.baseline}</code>，
            策略 {shownRelease.strategy}，每批 {shownRelease.batchSize} 台 · {shownRelease.message ?? "进行中，所有进度实时持久化"}
          </p>
          {shownRelease.compatBlocked && (
            <p className="error">兼容约束阻止推进：依赖者必须先更新到兼容版本，之后到审查中选择“继续”。</p>
          )}
          {releaseDeps && releaseDeps.pending.length > 0 && (
            <div className="deps">
              <strong>未完成依赖：</strong>
              <ul>
                {releaseDeps.pending.map((p, i) => <li key={i}>{p}</li>)}
              </ul>
            </div>
          )}
          {shownRelease.rollbackNote && (
            <p className="hint">回退评估：{shownRelease.rollbackNote}</p>
          )}
          {shownRelease.status === "paused" && (
            <PausedReview snap={snap} release={shownRelease} onError={setError} />
          )}
          <div className="batches">
            {batches.map((b) => (
              <BatchView key={b.id} batch={b} ops={snap.ops}
                reservations={snap.reservations.filter((r) => r.batchId === b.id)} />
            ))}
          </div>
        </section>
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
      <h3><OctagonAlert size={16} /> 暂停审查：{release.compatBlocked ? "兼容约束未满足，等待依赖者先更新" : "第 " + (pausedBatch ? pausedBatch.index + 1 : "-") + " 批需要人工决定"}</h3>
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

const ruleModeLabel: Record<string, string> = {
  update_first: "依赖者必须先更新",
  stay_old: "依赖者可保持旧版",
  pause: "推进前需人工审查",
};

// ZonesCard is the zone capacity entry: for every fault zone the operator
// configures each service's minimum available count and the shared
// cross-service pool, and sees live occupancy from held commitments.
function ZonesCard({ snap, onError }: { snap: Snapshot; onError: (m: string) => void }) {
  const serviceName = (id: string) => snap.services.find((s) => s.id === id)?.name ?? id;
  return (
    <div className="card">
      <h2><Boxes size={18} /> 故障区域与容量入口（{snap.zones.length}）</h2>
      {snap.zones.map((z) => (
        <ZonePolicyEditor key={z.id} zone={z} snap={snap} serviceName={serviceName} onError={onError} />
      ))}
      {snap.zones.length === 0 && <p className="empty">暂无故障区域：初始化环境后自动创建两个区域。</p>}
      <p className="hint">批次扩批前必须通过区域容量审查：服务可用数不低于下限，且区域内跨服务占用不超过共享容量，否则批次等待并显示原因。</p>
    </div>
  );
}

function ZonePolicyEditor({
  zone,
  snap,
  serviceName,
  onError,
}: {
  zone: Zone;
  snap: Snapshot;
  serviceName: (id: string) => string;
  onError: (m: string) => void;
}) {
  const policy = snap.policies.find((p) => p.zoneId === zone.id);
  const [minAvailable, setMinAvailable] = useState<Record<string, number>>(policy?.minAvailable ?? {});
  const [sharedMax, setSharedMax] = useState<number>(policy?.sharedMax ?? 0);
  const [saved, setSaved] = useState("");
  useEffect(() => {
    setMinAvailable(policy?.minAvailable ?? {});
    setSharedMax(policy?.sharedMax ?? 0);
  }, [policy?.zoneId, policy?.sharedMax, JSON.stringify(policy?.minAvailable)]);
  const zoneInsts = snap.instances.filter((i) => i.zoneId === zone.id);
  const held = snap.reservations.filter((r) => r.zoneId === zone.id && r.state === "held");
  const heldCount = held.reduce((n, r) => n + r.members.length, 0);
  const submit = async () => {
    try {
      await setCapacityPolicy(zone.id, { minAvailable, sharedMax: Number(sharedMax) });
      setSaved("已保存 " + new Date().toLocaleTimeString("zh-CN", { hour12: false }));
    } catch (e) {
      onError((e as Error).message);
    }
  };
  return (
    <div className="zone">
      <h3>{zone.name} <span className="muted">（{zoneInsts.length} 台实例，当前被承诺占用 {heldCount} 台）</span></h3>
      <table>
        <thead><tr><th>服务</th><th>区域内实例</th><th>最低可用数量</th></tr></thead>
        <tbody>
          {snap.services.map((s) => {
            const count = zoneInsts.filter((i) => i.serviceId === s.id).length;
            return (
              <tr key={s.id}>
                <td>{s.name}</td>
                <td>{count} 台</td>
                <td>
                  <input
                    type="number"
                    min={0}
                    max={count}
                    value={minAvailable[s.id] ?? 0}
                    onChange={(e) => setMinAvailable({ ...minAvailable, [s.id]: Number(e.target.value) })}
                  />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <label className="shared-max">跨服务共享容量（0 为不限）
        <input type="number" min={0} value={sharedMax} onChange={(e) => setSharedMax(Number(e.target.value))} />
      </label>
      <div className="actions">
        <button className="primary" onClick={submit}>保存区域容量</button>
        {saved && <span className="hint">{saved}</span>}
      </div>
      {held.length > 0 && (
        <p className="hint">持有中承诺：{held.map((r) => r.id + "（" + serviceName(r.serviceId) + "，占 " + r.members.length + " 台）").join("；")}</p>
      )}
    </div>
  );
}

// ReservationsCard lists every capacity commitment with its grant and
// release evidence, so each occupation and release can be audited against
// the real instance records.
function ReservationsCard({ snap }: { snap: Snapshot }) {
  const serviceName = (id: string) => snap.services.find((s) => s.id === id)?.name ?? id;
  const zoneName = (id: string) => snap.zones.find((z) => z.id === id)?.name ?? id;
  const sorted = [...snap.reservations].sort((a, b) => (a.createdAt < b.createdAt ? -1 : 1)).reverse();
  return (
    <div className="card">
      <h2><FileClock size={18} /> 区域容量承诺（{snap.reservations.length}）</h2>
      {sorted.length === 0 && <p className="empty">暂无容量承诺：批次通过容量审查扩批后，这里会显示可查询的承诺与其释放依据。</p>}
      {sorted.slice(0, 12).map((r) => (
        <div key={r.id} className={"reservation " + r.state}>
          <div className="res-head">
            <strong>{r.id}</strong>
            <span className={"pill " + (r.state === "held" ? "b-observing" : "b-succeeded")}>
              {r.state === "held" ? "持有中" : "已释放"}
            </span>
            <span className="when">{zoneName(r.zoneId)} · {serviceName(r.serviceId)} · 占用 {r.members.join("、")}</span>
          </div>
          <ul className="res-basis">
            {r.basis.map((b, i) => <li key={i}>承诺依据：{b}</li>)}
            {r.releaseReason && <li className="rel">释放原因：{r.releaseReason}（{timeShort(r.releasedAt)}）</li>}
            {(r.releaseBasis ?? []).map((b, i) => <li key={"r" + i} className="rel">释放依据：{b}</li>)}
          </ul>
        </div>
      ))}
    </div>
  );
}

function ServicesCard({ snap, onError }: { snap: Snapshot; onError: (m: string) => void }) {
  return (
    <div className="card">
      <h2><Boxes size={18} /> 虚拟服务（{snap.services.length}）</h2>
      <table>
        <thead><tr><th>服务</th><th>实例数</th><th>运行版本</th><th></th></tr></thead>
        <tbody>
          {snap.services.map((s) => {
            const insts = snap.instances.filter((i) => i.serviceId === s.id);
            const versions = [...new Set(insts.map((i) => i.currentVersion))];
            return (
              <tr key={s.id}>
                <td>{s.name}</td>
                <td>{insts.length} 台</td>
                <td>{versions.length ? versions.map((v) => <code key={v}>{v} </code>) : "—"}</td>
                <td>
                  <button onClick={() => addInstance(s.id).catch((e) => onError(e.message))}>
                    <Plus size={14} /> 添加实例
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <p className="hint">新实例创建在服务基线版本上，全部为有持久化记录的本地虚拟实例。</p>
    </div>
  );
}

function RulesCard({ snap, onError }: { snap: Snapshot; onError: (m: string) => void }) {
  const [providerId, setProviderId] = useState("");
  const [dependentId, setDependentId] = useState("");
  const [dependentVersion, setDependentVersion] = useState("");
  const [maxProviderVersion, setMaxProviderVersion] = useState("");
  const [mode, setMode] = useState("update_first");
  const [note, setNote] = useState("");
  const name = (id: string) => snap.services.find((s) => s.id === id)?.name ?? id;
  const submit = async () => {
    try {
      await addRule({
        providerId, dependentId, mode: mode as Snapshot["rules"][number]["mode"],
        dependentVersion: dependentVersion || undefined,
        maxProviderVersion: maxProviderVersion || undefined,
        note: note || undefined,
      });
      setNote("");
    } catch (e) {
      onError((e as Error).message);
    }
  };
  return (
    <div className="card">
      <h2><Link2 size={18} /> 服务间兼容约束（{snap.rules.length}）</h2>
      <table>
        <thead><tr><th>更新服务</th><th>依赖者</th><th>依赖者版本</th><th>兼容范围</th><th>处理方式</th></tr></thead>
        <tbody>
          {snap.rules.map((r) => (
            <tr key={r.id}>
              <td>{name(r.providerId)}</td>
              <td>{name(r.dependentId)}</td>
              <td><code>{r.dependentVersion || "任意"}</code></td>
              <td><code>{r.minProviderVersion || "…"} ~ {r.maxProviderVersion || "…"}</code></td>
              <td>{ruleModeLabel[r.mode] ?? r.mode}{r.note ? "（" + r.note + "）" : ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <details className="add-rule">
        <summary>新增兼容约束</summary>
        <div className="rule-form">
          <label>更新服务
            <select value={providerId} onChange={(e) => setProviderId(e.target.value)}>
              <option value="">选择…</option>
              {snap.services.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
          </label>
          <label>依赖者
            <select value={dependentId} onChange={(e) => setDependentId(e.target.value)}>
              <option value="">选择…</option>
              {snap.services.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
          </label>
          <label>依赖者版本（空为任意）
            <input value={dependentVersion} onChange={(e) => setDependentVersion(e.target.value)} placeholder="v1.0.0" />
          </label>
          <label>兼容到的最高版本
            <input value={maxProviderVersion} onChange={(e) => setMaxProviderVersion(e.target.value)} placeholder="v1.9.9" />
          </label>
          <label>不满足时
            <select value={mode} onChange={(e) => setMode(e.target.value)}>
              <option value="update_first">依赖者必须先更新（阻止扩批）</option>
              <option value="stay_old">依赖者可保持旧版（仅提示）</option>
              <option value="pause">暂停推进待人工审查</option>
            </select>
          </label>
          <label>备注
            <input value={note} onChange={(e) => setNote(e.target.value)} />
          </label>
          <button className="primary" disabled={!providerId || !dependentId} onClick={submit}>保存约束</button>
        </div>
      </details>
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

function BatchView({ batch, ops, reservations }: { batch: Batch; ops: Snapshot["ops"]; reservations: Snapshot["reservations"] }) {
  const pendingOps = ops.filter((o) => o.batchId === batch.id);
  return (
    <div className={"batch ph-" + batch.phase}>
      <div className="batch-head">
        <strong>第 {batch.index + 1} 批</strong>
        <span className={"pill b-" + batch.phase}>{phaseLabel[batch.phase] ?? batch.phase}</span>
        <span className="when">{timeShort(batch.startedAt)} → {timeShort(batch.finishedAt)}</span>
      </div>
      {batch.capacityWaiting && (
        <p className="capacity-waiting">等待区域容量：{batch.capacityWaiting}</p>
      )}
      {reservations.length > 0 && (
        <div className="preflight">
          <strong>区域容量承诺（本批占用实例）：</strong>
          <ul>
            {reservations.map((r) => (
              <li key={r.id} className={r.state === "held" ? "ok" : ""}>
                {r.id}（{r.state === "held" ? "持有中" : "已释放"}）：占用 {r.members.join("、")}
                {r.releaseReason ? "；释放依据：" + r.releaseReason : ""}
              </li>
            ))}
          </ul>
        </div>
      )}
      {batch.preflight && (
        <div className="preflight">
          <strong>生效前依据（{timeShort(batch.preflight.checkedAt)} 评估）：</strong>
          <ul>
            {batch.preflight.findings.map((f, i) => (
              <li key={i} className={f.compatible ? "ok" : "bad"}>
                {f.compatible ? "兼容" : f.blocking ? "阻止" : "待审查"}：{f.detail}
              </li>
            ))}
            {batch.preflight.findings.length === 0 && <li className="ok">无跨服务约束涉及本批次</li>}
            {batch.preflight.healthBasis.map((h, i) => (
              <li key={"h" + i} className="ok">健康依据：{h}</li>
            ))}
          </ul>
        </div>
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
