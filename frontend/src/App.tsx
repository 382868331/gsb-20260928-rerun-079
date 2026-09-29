import { useCallback, useEffect, useMemo, useState } from "react";
import { api, fetchState, type ScenarioOptions } from "./api";
import type { Release, Snapshot } from "./types";
import { VersionsPanel } from "./components/VersionsPanel";
import { InstancesPanel } from "./components/InstancesPanel";
import { BatchesPanel } from "./components/BatchesPanel";
import { EventTimeline } from "./components/EventTimeline";

const SCENARIOS: Record<string, ScenarioOptions> = {
	half: {
		instance_count: 4,
		unhealthy_idx: [0, 1],
		probe_delay_ms: { 0: 200, 1: 200 },
	},
	cancel: { instance_count: 4 },
	late: {
		instance_count: 4,
		probe_delay_ms: { 0: 5000 },
	},
	failUpdate: { instance_count: 4, fail_update_idx: [0] },
};

export function App() {
	const [snap, setSnap] = useState<Snapshot | null>(null);
	const [error, setError] = useState<string>("");
	const [paused, setPaused] = useState(false);
	const [batchSpec, setBatchSpec] = useState("50,50");

	const refresh = useCallback(async () => {
		try {
		const s = await fetchState();
		setSnap(s);
		setError("");
		} catch (e) {
		setError((e as Error).message);
		}
	}, []);

	useEffect(() => {
		refresh();
		const t = setInterval(refresh, 600);
		return () => clearInterval(t);
	}, [refresh]);

	const active = useMemo<Release | undefined>(() => {
		if (!snap) return undefined;
		const live = snap.releases.filter((r) =>
			["pending", "rolling_out", "paused", "failing", "rolling_back"].includes(r.status),
		);
		return live[live.length - 1];
	}, [snap]);

	const run = async (fn: () => Promise<void>) => {
		try {
		await fn();
			await refresh();
		} catch (e) {
			setError((e as Error).message);
		}
	};

	const percents = useMemo(
		() =>
			batchSpec
				.split(",")
				.map((s) => parseInt(s.trim(), 10))
				.filter((n) => !Number.isNaN(n)),
		[batchSpec],
	);

	const targetVersion = snap?.versions.find((v) => v.number === "v2.0.0")?.id;

	return (
		<div className="app">
			<header className="topbar">
				<div>
					<h1>软件发布演练平台</h1>
					<p className="sub">本地虚拟实例 · 批次更新 / 健康观察 / 失败回退 · 不实际部署</p>
				</div>
				<div className="controls">
					<button onClick={() => run(async () => {
						setPaused(!paused);
						paused ? await api.resume() : await api.pause();
					})}>
						{paused ? "▶ 继续" : "⏸ 暂停"}
					</button>
					<button onClick={refresh}>⟳ 刷新真实进度</button>
				</div>
			</header>

			{error && <div className="error">{error}</div>}

			<section className="panel" id="scenario">
				<h2>演练场景（重置并播种）</h2>
				<div className="row">
					{Object.entries(SCENARIOS).map(([key, opts]) => (
						<button
							key={key}
							onClick={() => run(() => api.setup(opts))}
							className="secondary"
						>
							{scenarioLabel(key)}
						</button>
					))}
				</div>
				<div className="row">
					<label>
						批次百分比（逗号分隔，合计 100）：
						<input
							value={batchSpec}
							onChange={(e) => setBatchSpec(e.target.value)}
							style={{ width: 140, marginLeft: 8 }}
						/>
					</label>
					<button
						disabled={!targetVersion || !!active || percents.reduce((a, b) => a + b, 0) !== 100}
						onClick={() => run(() => api.startRelease(targetVersion!, percents))}
					>
						🚀 开始发布 v2.0.0
					</button>
					<button
						disabled={!active}
						onClick={() => run(() => api.cancel(active!.id))}
						className="danger"
					>
						🛑 取消发布（停止扩批并回退）
					</button>
				</div>
			</section>

			{snap && (
				<main className="grid">
					<VersionsPanel versions={snap.versions} />
					<InstancesPanel instances={snap.instances} versions={snap.versions} faults={snap.faults} />
					<BatchesPanel batches={snap.batches} releases={snap.releases} active={active} />
					<EventTimeline events={snap.events} />
				</main>
			)}
		</div>
	);
}

function scenarioLabel(key: string): string {
	switch (key) {
		case "half":
			return "半批失败（50% 节点不健康）";
		case "cancel":
		return "健康发布（可中途取消）";
		case "late":
		return "迟到健康结果";
		case "failUpdate":
		return "更新失败回退";
		default:
			return key;
	}
}
