import type { Version } from "../types";

export function VersionsPanel({ versions }: { versions: Version[] }) {
	return (
		<section className="panel">
			<h2>版本（独立存档）</h2>
			<table>
				<thead>
					<tr><th>ID</th><th>版本号</th><th>制品（本地）</th></tr>
				</thead>
				<tbody>
					{versions.map((v) => (
						<tr key={v.id}>
							<td className="mono">{v.id}</td>
							<td>{v.number}</td>
							<td className="mono small">{v.artifact}</td>
						</tr>
					))}
					{versions.length === 0 && (
						<tr><td colSpan={3} className="muted">先在上方播种一个演练场景</td></tr>
					)}
				</tbody>
			</table>
		</section>
	);
}
