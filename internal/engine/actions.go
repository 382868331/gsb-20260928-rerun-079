package engine

import (
	"fmt"

	"releaselab/internal/store"
)

// completeDueActions finishes update/rollback actions whose virtual
// duration elapsed, then schedules their health probe.
func (e *Engine) completeDueActions(tx *store.Tx, r *store.Release, now int64) {
	for i := range tx.Instances() {
		in := &tx.Instances()[i]
		if in.ReleaseID != r.ID {
			continue
		}
		if in.State != StatusInstanceUpdating && in.State != StatusInstanceRollingBack {
			continue
		}
		due := in.ActionStartMS + int64(r.ActionDurationMS)
		if now < due {
			continue
		}
		f := faultFor(tx, in.ID, in.ActionVersion)
		willFail := f != nil && f.FailUpdate
		rollbackAction := in.State == StatusInstanceRollingBack
		if willFail {
			// Update never lands: instance keeps the old version. The
			// generation token is invalidated so any late probe for the
			// attempted version is discarded.
			oldToken := in.GenerationToken
			in.GenerationToken = ""
			in.DesiredVersion = ""
			in.Action = ""
			in.Health = Unhealthy
			in.State = StatusInstanceUpdateFail
			in.UpdatedAtMS = now
			tx.SaveInstance(*in)
			e.discardProbe(oldToken)
			e.recordActionOutcome(tx, r, in, false, now)
			actionName := "update"
			if rollbackAction {
				actionName = "rollback"
			}
			e.log(tx, now, "instance_action_failed",
				fmt.Sprintf("%s: %s to %s failed; remains on %s", in.Name, actionName, in.ActionVersion, in.CurrentVersion),
				r.ID, in.BatchID, in.ID)
			continue
		}

		// Action lands: new current version, probe the new generation.
		in.CurrentVersion = in.ActionVersion
		in.Health = HealthUnknown
		in.State = StatusInstanceObserving
		in.Action = ""
		in.DesiredVersion = ""
		in.UpdatedAtMS = now
		tx.SaveInstance(*in)
		e.log(tx, now, "instance_action_done",
			fmt.Sprintf("%s: now running %s; probing health", in.Name, in.CurrentVersion),
			r.ID, in.BatchID, in.ID)
		e.scheduleProbe(tx, in, f, now, int64(r.HealthWaitMS))
	}
}

func (e *Engine) recordActionOutcome(tx *store.Tx, r *store.Release, in *store.Instance, _ bool, now int64) {
	b := tx.Batch(in.BatchID)
	if b == nil {
		return
	}
	switch b.Phase {
	case PhaseRollout:
		if b.Status == BatchUpdating {
			b.UpdateFailedIDs = append(b.UpdateFailedIDs, in.ID)
			tx.SaveBatch(*b)
		}
	case PhaseRollback:
		// Failed rollback instance stays off-base; batch still tracks it.
		b.UpdateFailedIDs = append(b.UpdateFailedIDs, in.ID)
		tx.SaveBatch(*b)
	}
}

// scheduleProbe arms the virtual health check. The probe carries the
// generation token; when it lands the token must still match.
func (e *Engine) scheduleProbe(tx *store.Tx, in *store.Instance, f *store.InstanceFault, now, waitMS int64) {
	delay := int64(waitMS * 60 / 100) // healthy verdict arrives within the window by default
	healthy := true
	if f != nil {
		if f.Unhealthy {
			healthy = false
		}
		if f.ProbeDelay > 0 {
			delay = int64(f.ProbeDelay)
		}
	}
	tp := pendingProbe{
		token:      in.GenerationToken,
		instanceID: in.ID,
		dueMS:      now + delay,
		healthy:    healthy,
	}
	e.mu.Lock()
	e.probes[tp.token] = tp
	e.mu.Unlock()
}

func (e *Engine) discardProbe(token string) {
	if token == "" {
		return
	}
	e.mu.Lock()
	delete(e.probes, token)
	e.mu.Unlock()
}

func (e *Engine) clock() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nowMS == 0 {
		e.nowMS = 1_700_000_000_000
	}
	return e.nowMS
}

func (e *Engine) deliverDueProbes(now int64) {
	e.mu.Lock()
	due := []pendingProbe{}
	for token, p := range e.probes {
		if p.dueMS <= now {
			due = append(due, p)
			delete(e.probes, token)
		}
	}
	e.mu.Unlock()

	for _, p := range due {
		_ = e.st.Mutate(func(tx *store.Tx) error {
			in := tx.Instance(p.instanceID)
			if in == nil {
				return nil
			}
			// Generation guard: a late result only counts while the
			// instance still awaits this exact generation. If the
			// instance moved on (next version, rollback, failed update),
			// the result is dropped and explicitly logged.
			if in.GenerationToken != p.token {
				e.log(tx, now, "probe_stale_discarded",
					fmt.Sprintf("%s: late health result for a superseded generation discarded (not attributed to current version %s)",
						in.Name, in.CurrentVersion),
					in.ReleaseID, in.BatchID, in.ID)
				return nil
			}
			verdict := Healthy
			if !p.healthy {
				verdict = Unhealthy
			}
			in.Health = verdict
			tx.SaveInstance(*in)
			b := tx.Batch(in.BatchID)
			if b != nil && (b.Status == BatchObserving || b.Status == BatchRollingBack) {
				applyProbeToBatch(b, p.healthy)
				if b.Phase == PhaseRollback && p.healthy {
					in.State = StatusInstanceReady
					in.Health = Healthy
					in.GenerationToken = ""
				}
				tx.SaveBatch(*b)
			}
			e.log(tx, now, "probe_result",
				fmt.Sprintf("%s: %s on %s (generation verified)", in.Name, verdict, in.CurrentVersion),
				in.ReleaseID, in.BatchID, in.ID)
			return nil
		})
	}
}

func applyProbeToBatch(b *store.Batch, healthy bool) {
	if b.PendingCount > 0 {
		b.PendingCount--
	}
	if healthy {
		b.HealthyCount++
	} else {
		b.UnhealthyCount++
	}
}
