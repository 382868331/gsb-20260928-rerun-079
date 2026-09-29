import type { Instance, InstanceFault, Version } from "../types";

interface Props {
	instances: Instance[];
	versions: Version[];
	faults: InstanceFault[];
}

function versionNumber(versions: Version[], id: string): string {
	return versions.find((v) => v.id === id)?.number ?? id ?? "-";
}

const STATE_LABEL: Record<string, string> = {
	ready: "就绪",
	updating: "更新中",
	observing: "健康观察",
	update_failed: "更新失败",
	rolling_back: "回退中",
};

export function InstancesPanel({ instances, versions, faults }: Props) {
	return (
		<section className="panel wide">
			<h2>虚拟实例（独立存档）</h2>
			<table>
				<thead>
					<tr>
						<th>实例</th><th>当前版本</th><th>状态</th><th>健康</th>
						<th>所属发布/批次</th><th>代际令牌</th><th>注入故障</th>
					</tr>
				</thead>
				<tbody>
					{instances.map((inst) => {
						const fault = faults.find((f) => f.instance_id === inst.id);
						return (
							<tr key={inst.id}>
								<td className="mono">{inst.name}</td>
								<td>{versionNumber(versions, inst.current_version)}</td>
								<td><span className={`pill state-${inst.state}`}>{STATE_LABEL[inst.state] ?? inst.state}</span></td>
								<td><span className={`pill health-${inst.health}`}>{inst.health}</span></td>
								<td className="small mono">
									{inst.release_id ? `${inst.release_id.slice(0, 16)}… / ${inst.batch_id ?? "-"}` : "-"}
								</td>
								<td className="small mono">{inst.generation_token ? inst.generation_token.slice(0, 8) : "-"}</td>
								<td className="small">
									{fault ? [
										fault.fail_update ? "更新必失败" : "",
										fault.unhealthy ? "探针不健康" : "",
										fault.probe_delay_ms ? `探针迟到 ${fault.probe_delay_ms}ms` : "",
									].filter(Boolean).join(" / ") : "-"}
								</td>
							</tr>
						);
					})}
				</tbody>
			</table>
		</section>
	);
}
