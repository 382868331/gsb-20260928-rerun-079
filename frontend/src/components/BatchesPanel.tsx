import type { Batch, Release } from "../types";

interface Props {
	batches: Batch[];
	releases: Release[];
	active?: Release;
}

const STATUS_LABEL: Record<string, string> = {
	pending: "等待",
	updating: "更新中",
	observing: "观察中",
	healthy: "健康通过",
	failed: "失败",
	rolling_back: "回退中",
	rolled_back: "已回退",
};

const RELEASE_LABEL: Record<string, string> = {
	pending: "待开始",
	rolling_out: "滚动发布中",
	paused: "已暂停",
	failing: "触发失败 → 准备回退",
	rolling_back: "回退中",
	succeeded: "发布成功",
	cancelled: "已取消（已回退）",
	failed: "失败（已回退到基线）",
};

export function BatchesPanel({ batches, releases, active }: Props) {
	return (
		<section className="panel wide">
			<h2>
				发布与批次（独立存档）
				{active && (
					<span className={`pill release-${active.status}`} style={{ marginLeft: 12 }}>
						{RELEASE_LABEL[active.status] ?? active.status}
					</span>
				)}
			</h2>
			{releases.length === 0 && <p className="muted">尚无发布。</p>}
			{releases.slice().reverse().map((r) => {
				const relBatches = batches
					.filter((b) => b.release_id === r.id)
					.sort((a, b) =>
						a.phase === b.phase ? a.index - b.index : a.phase === "rollout" ? -1 : 1,
					);
				return (
					<div key={r.id} className="release-card">
						<div className="release-head">
							<span className="mono small">{r.id}</span>
							<span className={`pill release-${r.status}`}>
								{RELEASE_LABEL[r.status] ?? r.status}
							</span>
							{r.cancel_requested && <span className="pill health-unhealthy">已请求取消</span>}
							{r.reason && <span className="small muted">{r.reason}</span>}
						</div>
						<div className="batch-track">
							{relBatches.map((b) => (
								<div key={b.id} className={`batch-chip batch-${b.status} phase-${b.phase}`}>
									<div className="batch-title">
										{b.phase === "rollout" ? `发布批次 ${b.index + 1}` : `回退批次 ${b.index + 1}`}
										<span className="small">（{b.instance_ids.length} 台）</span>
									</div>
									<div className="small">{STATUS_LABEL[b.status] ?? b.status}</div>
									<div className="small counts">
										<span className="ok">✓{b.healthy_count}</span>
										<span className="bad">✗{b.unhealthy_count}</span>
										<span className="muted">?{b.pending_count}</span>
										{b.update_failed_ids.length > 0 && (
											<span className="bad">更新失败 {b.update_failed_ids.length}</span>
										)}
									</div>
								</div>
							))}
						</div>
					</div>
				);
			})}
		</section>
	);
}
