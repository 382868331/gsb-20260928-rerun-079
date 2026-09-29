export interface Version {
	id: string;
	number: string;
	artifact: string;
	created_at_ms: number;
}

export interface Instance {
	id: string;
	name: string;
	current_version: string;
	desired_version?: string;
	state: string;
	health: string;
	release_id?: string;
	batch_id?: string;
	action?: string;
	action_version?: string;
	generation_token?: string;
	action_start_ms?: number;
	updated_at_ms: number;
}

export interface InstanceFault {
	instance_id: string;
	version_id: string;
	fail_update: boolean;
	unhealthy: boolean;
	probe_delay_ms: number;
}

export interface Batch {
	id: string;
	release_id: string;
	index: number;
	phase: "rollout" | "rollback";
	status: string;
	instance_ids: string[];
	healthy_count: number;
	unhealthy_count: number;
	pending_count: number;
	update_failed_ids: string[];
	started_at_ms?: number;
	deadline_ms?: number;
	finished_at_ms?: number;
}

export interface Release {
	id: string;
	target_version_id: string;
	base_version_id: string;
	strategy: string;
	batch_percent: number[];
	status: string;
	current_batch_index: number;
	rollback_batch_index: number;
	rollback_planned: boolean;
	health_wait_ms: number;
	action_duration_ms: number;
	cancel_requested: boolean;
	reason?: string;
	created_at_ms: number;
	updated_at_ms: number;
	finished_at_ms?: number;
}

export interface Event {
	seq: number;
	at_ms: number;
	release_id?: string;
	batch_id?: string;
	instance?: string;
	kind: string;
	message: string;
}

export interface Snapshot {
	versions: Version[];
	instances: Instance[];
	faults: InstanceFault[];
	releases: Release[];
	batches: Batch[];
	events: Event[];
	now_ms: number;
}
