package core

import (
	"errors"
	"time"

	"github.com/muq9738/Vantry/internal/telemetry"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

type runTelemetryEventData struct {
	Telemetry api.RunTelemetry `json:"telemetry"`
}

type runTelemetryState struct {
	attemptID   string
	lastSeq     int64
	phase       *telemetry.PhaseTracker
	collector   *telemetry.RunCollector
	lastEmitted *api.RunTelemetry
	finalized   *api.RunTelemetry
}

func (c *Core) newRunTelemetryState(eff effect) *runTelemetryState {
	return &runTelemetryState{
		attemptID: eff.attemptID,
		phase:     telemetry.NewPhaseTracker(),
		collector: telemetry.NewRunCollector(telemetry.RunMetadata{
			RunID:      eff.runID,
			AttemptID:  eff.attemptID,
			ThreadID:   eff.threadID,
			ProviderID: eff.providerID,
			Model:      eff.model,
		}, time.Now().UTC()),
	}
}

func (c *Core) startRunTelemetry(eff effect) {
	c.telemetryMu.Lock()
	defer c.telemetryMu.Unlock()
	if c.liveTelemetry == nil {
		c.liveTelemetry = make(map[string]*runTelemetryState)
	}
	if existing, ok := c.liveTelemetry[eff.runID]; ok && existing.attemptID == eff.attemptID {
		return
	}
	c.liveTelemetry[eff.runID] = c.newRunTelemetryState(eff)
}

func (c *Core) observeProviderTelemetry(t *txn, eff effect, event provider.Event) error {
	c.telemetryMu.Lock()
	if c.liveTelemetry == nil {
		c.liveTelemetry = make(map[string]*runTelemetryState)
	}
	st := c.liveTelemetry[eff.runID]
	if st == nil || st.attemptID != eff.attemptID {
		st = c.newRunTelemetryState(eff)
		c.liveTelemetry[eff.runID] = st
	}
	if event.Sequence > 0 && event.Sequence <= st.lastSeq {
		c.telemetryMu.Unlock()
		return nil
	}
	st.lastSeq = event.Sequence
	phaseSnap := st.phase.Apply(event)
	st.collector.Observe(event)
	cur := buildRunTelemetry(phaseSnap, st.collector.Metrics(time.Now().UTC()))
	emit := shouldEmitTelemetry(st.lastEmitted, cur)
	c.telemetryMu.Unlock()

	if !emit {
		return nil
	}
	if _, err := t.event(eventRunTelemetry, eff.threadID, eff.runID, eff.attemptID,
		runTelemetryEventData{Telemetry: cur}); err != nil {
		return err
	}
	curCopy := cur
	c.telemetryMu.Lock()
	if curSt, ok := c.liveTelemetry[eff.runID]; ok && curSt == st {
		curSt.lastEmitted = &curCopy
	}
	c.telemetryMu.Unlock()
	return nil
}

func (c *Core) finalizeRunTelemetry(eff effect, providerErr error) {
	c.telemetryMu.Lock()
	st := c.liveTelemetry[eff.runID]
	if st == nil || st.attemptID != eff.attemptID {
		c.telemetryMu.Unlock()
		return
	}
	var phaseSnap telemetry.PhaseSnapshot
	switch {
	case providerErr == nil:
		phaseSnap = st.phase.MarkCompleted()
	case errors.Is(providerErr, provider.ErrExecutionCancelled):
		phaseSnap = st.phase.MarkCancelled()
	default:
		phaseSnap = st.phase.MarkFailed(providerErr.Error())
	}
	metrics := st.collector.Finalize(time.Now().UTC())
	fin := buildRunTelemetry(phaseSnap, metrics)
	if hasMeteredTelemetry(fin) {
		finCopy := fin
		st.finalized = &finCopy
	}
	c.telemetryMu.Unlock()

	if c.ledger != nil && (metrics.TotalTokens > 0 || metrics.LastContextTokens > 0 || metrics.TurnCount > 0 || metrics.CostUSD > 0) {
		_ = c.ledger.Record(metrics)
	}
}

func (c *Core) clearLiveRunTelemetry(runID, attemptID string) {
	c.telemetryMu.Lock()
	defer c.telemetryMu.Unlock()
	if st, ok := c.liveTelemetry[runID]; ok && st.attemptID == attemptID {
		delete(c.liveTelemetry, runID)
	}
}

func (c *Core) clearLiveRunTelemetryByID(runID string) {
	c.telemetryMu.Lock()
	defer c.telemetryMu.Unlock()
	delete(c.liveTelemetry, runID)
}

func (c *Core) runTelemetrySnapshot(runID string) *api.RunTelemetry {
	c.telemetryMu.RLock()
	st := c.liveTelemetry[runID]
	if st != nil {
		if st.finalized != nil {
			fin := *st.finalized
			c.telemetryMu.RUnlock()
			if c.ledger != nil {
				if agg, ok := c.ledger.GetRun(runID); ok {
					if merged := runTelemetryFromMetrics(agg); merged != nil {
						merged.Phase = fin.Phase
						return merged
					}
				}
			}
			return &fin
		}
		cur := buildRunTelemetry(st.phase.Snapshot(), st.collector.Metrics(time.Now().UTC()))
		c.telemetryMu.RUnlock()
		if cur.Compacting || cur.Retrying || hasMeteredTelemetry(cur) {
			return &cur
		}
		return nil
	}
	c.telemetryMu.RUnlock()

	if c.ledger != nil {
		if m, ok := c.ledger.GetRun(runID); ok {
			return runTelemetryFromMetrics(m)
		}
	}
	return nil
}

func buildRunTelemetry(phase telemetry.PhaseSnapshot, metrics telemetry.RunMetrics) api.RunTelemetry {
	out := api.RunTelemetry{
		Phase:             string(phase.Phase),
		TurnCount:         metrics.TurnCount,
		RetryCount:        metrics.RetryCount,
		InputTokens:       metrics.InputTokens,
		OutputTokens:      metrics.OutputTokens,
		CacheReadTokens:   metrics.CacheReadTokens,
		CacheWriteTokens:  metrics.CacheWriteTokens,
		ReasoningTokens:   metrics.ReasoningTokens,
		TotalTokens:       metrics.TotalTokens,
		LastContextTokens: metrics.LastContextTokens,
		CostUSD:           metrics.CostUSD,
		CostEstimated:     metrics.CostEstimated,
		TTFTMs:            metrics.TTFTMs,
		DurationMs:        metrics.DurationMs,
	}
	if phase.Phase == telemetry.PhaseCompacting || phase.Compaction != nil {
		out.Compacting = true
		if phase.Compaction != nil {
			out.CompactionReason = phase.Compaction.Reason
		}
	}
	if phase.Phase == telemetry.PhaseRetrying || phase.Retry != nil {
		out.Retrying = true
		out.RetryAttempt = phase.RetryAttempt
		out.RetryMaxAttempts = phase.RetryMaxAttempts
		out.RetryReason = phase.RetryReason
		out.RetryWaitSeconds = phase.RetryWaitSeconds
		if phase.Retry != nil {
			if out.RetryAttempt == 0 {
				out.RetryAttempt = phase.Retry.Attempt
			}
			if out.RetryMaxAttempts == 0 {
				out.RetryMaxAttempts = phase.Retry.MaxAttempts
			}
			if out.RetryReason == "" {
				out.RetryReason = phase.Retry.Reason
			}
			if out.RetryWaitSeconds == 0 && phase.Retry.WaitSeconds > 0 {
				out.RetryWaitSeconds = int(phase.Retry.WaitSeconds + 0.5)
			}
		}
	}
	return out
}

func runTelemetryFromMetrics(m telemetry.RunMetrics) *api.RunTelemetry {
	t := api.RunTelemetry{
		TurnCount:         m.TurnCount,
		RetryCount:        m.RetryCount,
		InputTokens:       m.InputTokens,
		OutputTokens:      m.OutputTokens,
		CacheReadTokens:   m.CacheReadTokens,
		CacheWriteTokens:  m.CacheWriteTokens,
		ReasoningTokens:   m.ReasoningTokens,
		TotalTokens:       m.TotalTokens,
		LastContextTokens: m.LastContextTokens,
		CostUSD:           m.CostUSD,
		CostEstimated:     m.CostEstimated,
		TTFTMs:            m.TTFTMs,
		DurationMs:        m.DurationMs,
	}
	if !hasMeteredTelemetry(t) {
		return nil
	}
	return &t
}

func hasMeteredTelemetry(t api.RunTelemetry) bool {
	return t.TotalTokens > 0 || t.LastContextTokens > 0 || t.TurnCount > 0 || t.RetryCount > 0 || t.CostUSD > 0
}

func shouldEmitTelemetry(prev *api.RunTelemetry, cur api.RunTelemetry) bool {
	var p api.RunTelemetry
	if prev != nil {
		p = *prev
	}
	if cur.Compacting != p.Compacting || cur.CompactionReason != p.CompactionReason {
		return true
	}
	if cur.Retrying != p.Retrying || cur.RetryAttempt != p.RetryAttempt || cur.RetryWaitSeconds != p.RetryWaitSeconds || cur.RetryCount != p.RetryCount {
		return true
	}
	if (cur.TotalTokens > 0 || cur.LastContextTokens > 0) &&
		(cur.TotalTokens != p.TotalTokens ||
			cur.LastContextTokens != p.LastContextTokens ||
			cur.OutputTokens != p.OutputTokens ||
			cur.CacheReadTokens != p.CacheReadTokens) {
		return true
	}
	return false
}
