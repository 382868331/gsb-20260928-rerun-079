import type { Event } from "../types";

const KIND_HINT: Record<string, string> = {
	release_created: "created",
	release_succeeded: "ok",
	release_failed: "bad",
	release_cancelled: "warn",
	release_cancel_requested: "warn",
	batch_healthy: "ok",
	batch_failed: "bad",
	batch_observing: "info",
	batch_update_start: "info",
	batch_promote: "info",
	rollback_planned: "warn",
	rollback_batch_start: "warn",
	rollback_batch_done: "ok",
	probe_stale_discarded: "warn",
	probe_result: "info",
};

function fmtTime(ms: number): string {
	const d = new Date(ms);
	return d.toLocaleTimeString("zh-CN", { hour12: false }) + "." + String(d.getMilliseconds()).padStart(3, "0");
}

export function EventTimeline({ events }: { events: Event[] }) {
	const shown = events.slice(-80).reverse();
	return (
		<section className="panel wide">
			<h2>操作过程时间线</h2>
			<div className="timeline">
				{shown.map((ev) => (
					<div key={ev.seq} className={`event ${KIND_HINT[ev.kind] ?? "info"}`}>
						<span className="seq">#{ev.seq}</span>
						<span className="time">{fmtTime(ev.at_ms)}</span>
						<span className="msg">{ev.message}</span>
						{ev.instance && <span className="mono small tag">{ev.instance}</span>}
					</div>
				))}
				{shown.length === 0 && <p className="muted">暂无事件。</p>}
			</div>
		</section>
	);
}
