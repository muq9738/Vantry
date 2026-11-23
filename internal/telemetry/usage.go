// Package telemetry provides runtime usage accounting, fine-grained execution
// phase projection, rate-limit/quota recovery advising, and session title generation.
package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

const ledgerFileName = "usage-ledger.json"

// CostBreakdown holds per-category and total monetary cost in USD.
type CostBreakdown struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Add returns the sum of two cost breakdowns.
func (c CostBreakdown) Add(other CostBreakdown) CostBreakdown {
	out := CostBreakdown{
		Input:      c.Input + other.Input,
		Output:     c.Output + other.Output,
		CacheRead:  c.CacheRead + other.CacheRead,
		CacheWrite: c.CacheWrite + other.CacheWrite,
		Total:      c.Total + other.Total,
	}
	if out.Total == 0 {
		out.Total = out.Input + out.Output + out.CacheRead + out.CacheWrite
	}
	return out
}

// TurnUsage represents normalized token and cost metrics extracted from a single
// provider completion turn or compaction event.
type TurnUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	CacheWrite1h     int64 `json:"cacheWrite1h,omitempty"`
	// ReasoningTokens is tracked for observability; per Pi/OpenAI semantics it is
	// already included in OutputTokens and must not be added to TotalTokens twice.
	ReasoningTokens int64         `json:"reasoningTokens,omitempty"`
	TotalTokens     int64         `json:"totalTokens"`
	Cost            CostBreakdown `json:"cost"`
	CostEstimated   bool          `json:"costEstimated,omitempty"`
	Provider        string        `json:"provider,omitempty"`
	Model           string        `json:"model,omitempty"`
	ResponseModel   string        `json:"responseModel,omitempty"`
	ResponseID      string        `json:"responseId,omitempty"`
	StopReason      string        `json:"stopReason,omitempty"`
}

// Add accumulates another TurnUsage into a copy of u.
func (u TurnUsage) Add(other TurnUsage) TurnUsage {
	out := u
	out.InputTokens += other.InputTokens
	out.OutputTokens += other.OutputTokens
	out.CacheReadTokens += other.CacheReadTokens
	out.CacheWriteTokens += other.CacheWriteTokens
	out.CacheWrite1h += other.CacheWrite1h
	out.ReasoningTokens += other.ReasoningTokens
	out.TotalTokens += other.TotalTokens
	out.Cost = out.Cost.Add(other.Cost)
	out.CostEstimated = out.CostEstimated || other.CostEstimated
	if out.Provider == "" {
		out.Provider = other.Provider
	}
	if out.Model == "" {
		out.Model = other.Model
	}
	if other.ResponseModel != "" {
		out.ResponseModel = other.ResponseModel
	}
	if other.ResponseID != "" {
		out.ResponseID = other.ResponseID
	}
	if other.StopReason != "" {
		out.StopReason = other.StopReason
	}
	return out
}

type rawCostWire struct {
	Input           *float64 `json:"input"`
	Output          *float64 `json:"output"`
	CacheRead       *float64 `json:"cacheRead"`
	CacheReadSnake  *float64 `json:"cache_read"`
	CacheWrite      *float64 `json:"cacheWrite"`
	CacheWriteSnake *float64 `json:"cache_write"`
	Total           *float64 `json:"total"`
}

type rawUsageWire struct {
	Input      *int64 `json:"input"`
	Output     *int64 `json:"output"`
	CacheRead  *int64 `json:"cacheRead"`
	CacheWrite *int64 `json:"cacheWrite"`

	CacheWrite1h      *int64 `json:"cacheWrite1h"`
	CacheWrite1hSnake *int64 `json:"cache_write_1h"`
	Reasoning         *int64 `json:"reasoning"`
	ReasoningSnake    *int64 `json:"reasoning_tokens"`
	TotalTokensCamel  *int64 `json:"totalTokens"`
	TotalTokensSnake  *int64 `json:"total_tokens"`

	// Claude / OpenAI / generic provider fields.
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	PromptTokens             *int64 `json:"prompt_tokens"`
	CompletionTokens         *int64 `json:"completion_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheWriteInputTokens    *int64 `json:"cache_write_input_tokens"`
	CachedTokens             *int64 `json:"cached_tokens"`
	PromptCacheHitTokens     *int64 `json:"prompt_cache_hit_tokens"`

	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`

	Cost         *rawCostWire `json:"cost"`
	TotalCostUSD *float64     `json:"total_cost_usd"`
	CostUSD      *float64     `json:"costUsd"`
}

func (w *rawUsageWire) present() bool {
	if w == nil {
		return false
	}
	return w.Input != nil ||
		w.Output != nil ||
		w.CacheRead != nil ||
		w.CacheWrite != nil ||
		w.TotalTokensCamel != nil ||
		w.TotalTokensSnake != nil ||
		w.InputTokens != nil ||
		w.OutputTokens != nil ||
		w.PromptTokens != nil ||
		w.CompletionTokens != nil ||
		w.CacheReadInputTokens != nil ||
		w.CacheCreationInputTokens != nil ||
		w.CacheWriteInputTokens != nil ||
		w.CachedTokens != nil ||
		w.PromptCacheHitTokens != nil ||
		w.Cost != nil ||
		w.TotalCostUSD != nil ||
		w.CostUSD != nil
}

type rawEnvelopeWire struct {
	Type         string        `json:"type"`
	Provider     string        `json:"provider"`
	Model        string        `json:"model"`
	TotalCostUSD *float64      `json:"total_cost_usd"`
	CostUSD      *float64      `json:"costUsd"`
	Usage        *rawUsageWire `json:"usage"`

	Message *struct {
		ID            string        `json:"id"`
		Role          string        `json:"role"`
		Provider      string        `json:"provider"`
		Model         string        `json:"model"`
		ResponseModel string        `json:"responseModel"`
		ResponseID    string        `json:"responseId"`
		StopReason    string        `json:"stopReason"`
		StopReasonAlt string        `json:"stop_reason"`
		Usage         *rawUsageWire `json:"usage"`
	} `json:"message"`

	Result *struct {
		Usage        *rawUsageWire `json:"usage"`
		TotalCostUSD *float64      `json:"total_cost_usd"`
	} `json:"result"`

	Response *struct {
		ID       string        `json:"id"`
		Provider string        `json:"provider"`
		Model    string        `json:"model"`
		Usage    *rawUsageWire `json:"usage"`
	} `json:"response"`
}

// ExtractTurnUsage losslessly extracts token and cost metrics from Pi JSONL
// completion records (message_end, turn_end, compaction_end, message_update),
// Claude/Anthropic stream events, OpenAI-compatible responses, or a bare usage JSON object.
func ExtractTurnUsage(raw []byte) (TurnUsage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return TurnUsage{}, false
	}

	var env rawEnvelopeWire
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return TurnUsage{}, false
	}

	var (
		wire *rawUsageWire
		out  TurnUsage
	)
	out.Provider = strings.TrimSpace(env.Provider)
	out.Model = strings.TrimSpace(env.Model)

	if env.Message != nil {
		role := strings.ToLower(strings.TrimSpace(env.Message.Role))
		if role != "" && role != "assistant" && env.Usage == nil && (env.Result == nil || env.Result.Usage == nil) {
			return TurnUsage{}, false
		}
		if env.Message.Provider != "" {
			out.Provider = strings.TrimSpace(env.Message.Provider)
		}
		if env.Message.Model != "" {
			out.Model = strings.TrimSpace(env.Message.Model)
		}
		out.ResponseModel = strings.TrimSpace(env.Message.ResponseModel)
		out.ResponseID = firstString(env.Message.ResponseID, env.Message.ID)
		out.StopReason = firstString(env.Message.StopReason, env.Message.StopReasonAlt)
		if env.Message.Usage.present() {
			wire = env.Message.Usage
		}
	}

	if wire == nil && env.Result != nil && env.Result.Usage.present() {
		wire = env.Result.Usage
	}
	if wire == nil && env.Response != nil {
		if env.Response.Provider != "" && out.Provider == "" {
			out.Provider = strings.TrimSpace(env.Response.Provider)
		}
		if env.Response.Model != "" && out.Model == "" {
			out.Model = strings.TrimSpace(env.Response.Model)
		}
		if env.Response.ID != "" && out.ResponseID == "" {
			out.ResponseID = strings.TrimSpace(env.Response.ID)
		}
		if env.Response.Usage.present() {
			wire = env.Response.Usage
		}
	}
	if wire == nil && env.Usage.present() {
		wire = env.Usage
	}
	if wire == nil {
		var bare rawUsageWire
		if err := json.Unmarshal(trimmed, &bare); err == nil && bare.present() {
			wire = &bare
		}
	}
	if !wire.present() {
		return TurnUsage{}, false
	}

	out.InputTokens = firstInt64(wire.Input, wire.InputTokens, wire.PromptTokens)
	out.OutputTokens = firstInt64(wire.Output, wire.OutputTokens, wire.CompletionTokens)
	out.CacheReadTokens = firstInt64(
		wire.CacheRead,
		wire.CacheReadInputTokens,
		wire.CachedTokens,
		wire.PromptCacheHitTokens,
		nestedCachedTokens(wire),
	)
	out.CacheWriteTokens = firstInt64(
		wire.CacheWrite,
		wire.CacheCreationInputTokens,
		wire.CacheWriteInputTokens,
	)
	out.CacheWrite1h = firstInt64(wire.CacheWrite1h, wire.CacheWrite1hSnake)
	out.ReasoningTokens = firstInt64(wire.Reasoning, wire.ReasoningSnake, nestedReasoningTokens(wire))

	explicitTotal := firstInt64Ptr(wire.TotalTokensCamel, wire.TotalTokensSnake)
	if explicitTotal != nil && *explicitTotal > 0 {
		out.TotalTokens = *explicitTotal
	} else {
		// When prompt_tokens / input_tokens_details.cached_tokens is used without
		// an explicit total, cached_tokens is already part of prompt_tokens.
		cachedIncludedInPrompt := wire.Input == nil && wire.CacheRead == nil && wire.CacheReadInputTokens == nil &&
			(wire.PromptTokens != nil || wire.InputTokensDetails != nil || wire.PromptTokensDetails != nil || wire.PromptCacheHitTokens != nil)
		if cachedIncludedInPrompt {
			out.TotalTokens = out.InputTokens + out.OutputTokens + out.CacheWriteTokens
		} else {
			out.TotalTokens = out.InputTokens + out.OutputTokens + out.CacheReadTokens + out.CacheWriteTokens
		}
		if explicitTotal != nil && out.TotalTokens == 0 {
			out.TotalTokens = *explicitTotal
		}
	}

	hasExplicitCost := false
	if wire.Cost != nil {
		hasExplicitCost = true
		out.Cost = CostBreakdown{
			Input:      derefFloat64(wire.Cost.Input),
			Output:     derefFloat64(wire.Cost.Output),
			CacheRead:  firstFloat64(wire.Cost.CacheRead, wire.Cost.CacheReadSnake),
			CacheWrite: firstFloat64(wire.Cost.CacheWrite, wire.Cost.CacheWriteSnake),
			Total:      derefFloat64(wire.Cost.Total),
		}
		if out.Cost.Total == 0 {
			out.Cost.Total = out.Cost.Input + out.Cost.Output + out.Cost.CacheRead + out.Cost.CacheWrite
		}
	}
	if topCost := firstFloat64Ptr(wire.TotalCostUSD, wire.CostUSD, env.TotalCostUSD, env.CostUSD, nestedResultCost(env)); topCost != nil {
		hasExplicitCost = true
		if out.Cost.Total == 0 {
			out.Cost.Total = *topCost
		}
	}

	if !hasExplicitCost {
		modelRef := firstString(out.ResponseModel, out.Model)
		if out.Provider != "" && modelRef != "" && !strings.Contains(modelRef, "/") {
			modelRef = out.Provider + "/" + modelRef
		}
		if estimated, ok := EstimateCost(modelRef, out); ok {
			out.Cost = estimated
			out.CostEstimated = true
		}
	}

	return out, true
}

// ExtractEventUsage extracts TurnUsage from a provider.Event's raw JSON payload.
func ExtractEventUsage(event provider.Event) (TurnUsage, bool) {
	if len(event.Data) == 0 {
		return TurnUsage{}, false
	}
	return ExtractTurnUsage(event.Data)
}

type modelRate struct {
	match          string
	inputPerM      float64
	outputPerM     float64
	cacheReadPerM  float64
	cacheWritePerM float64
}

// Built-in fallback pricing table (USD per 1M tokens) used only when a generic
// provider omits explicit cost fields. Pi's own cost breakdown always wins.
var defaultModelRates = []modelRate{
	{match: "claude-opus-4", inputPerM: 15.0, outputPerM: 75.0, cacheReadPerM: 1.5, cacheWritePerM: 18.75},
	{match: "claude-sonnet-4", inputPerM: 3.0, outputPerM: 15.0, cacheReadPerM: 0.3, cacheWritePerM: 3.75},
	{match: "claude-3-7-sonnet", inputPerM: 3.0, outputPerM: 15.0, cacheReadPerM: 0.3, cacheWritePerM: 3.75},
	{match: "claude-3-5-sonnet", inputPerM: 3.0, outputPerM: 15.0, cacheReadPerM: 0.3, cacheWritePerM: 3.75},
	{match: "claude-haiku-4", inputPerM: 0.8, outputPerM: 4.0, cacheReadPerM: 0.08, cacheWritePerM: 1.0},
	{match: "claude-3-5-haiku", inputPerM: 0.8, outputPerM: 4.0, cacheReadPerM: 0.08, cacheWritePerM: 1.0},
	{match: "gpt-5-mini", inputPerM: 0.25, outputPerM: 2.0, cacheReadPerM: 0.025, cacheWritePerM: 0.25},
	{match: "gpt-5-nano", inputPerM: 0.05, outputPerM: 0.4, cacheReadPerM: 0.005, cacheWritePerM: 0.05},
	{match: "gpt-5", inputPerM: 1.25, outputPerM: 10.0, cacheReadPerM: 0.125, cacheWritePerM: 1.25},
	{match: "gpt-4o-mini", inputPerM: 0.15, outputPerM: 0.6, cacheReadPerM: 0.075, cacheWritePerM: 0.15},
	{match: "gpt-4o", inputPerM: 2.5, outputPerM: 10.0, cacheReadPerM: 1.25, cacheWritePerM: 2.5},
	{match: "o3-mini", inputPerM: 1.1, outputPerM: 4.4, cacheReadPerM: 0.55, cacheWritePerM: 1.1},
	{match: "o3", inputPerM: 2.0, outputPerM: 8.0, cacheReadPerM: 0.5, cacheWritePerM: 2.0},
	{match: "o4-mini", inputPerM: 1.1, outputPerM: 4.4, cacheReadPerM: 0.275, cacheWritePerM: 1.1},
	{match: "gemini-2.5-pro", inputPerM: 1.25, outputPerM: 10.0, cacheReadPerM: 0.31, cacheWritePerM: 1.25},
	{match: "gemini-2.5-flash", inputPerM: 0.3, outputPerM: 2.5, cacheReadPerM: 0.075, cacheWritePerM: 0.3},
	{match: "deepseek-reasoner", inputPerM: 0.55, outputPerM: 2.19, cacheReadPerM: 0.14, cacheWritePerM: 0.55},
	{match: "deepseek-r1", inputPerM: 0.55, outputPerM: 2.19, cacheReadPerM: 0.14, cacheWritePerM: 0.55},
	{match: "deepseek", inputPerM: 0.27, outputPerM: 1.10, cacheReadPerM: 0.07, cacheWritePerM: 0.27},
}

// EstimateCost computes an estimated USD breakdown for providers that report
// token counts without billing amounts.
func EstimateCost(model string, usage TurnUsage) (CostBreakdown, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return CostBreakdown{}, false
	}
	for _, rate := range defaultModelRates {
		if strings.Contains(lower, rate.match) {
			inCost := float64(usage.InputTokens) * rate.inputPerM / 1_000_000
			outCost := float64(usage.OutputTokens) * rate.outputPerM / 1_000_000
			readCost := float64(usage.CacheReadTokens) * rate.cacheReadPerM / 1_000_000
			writeCost := float64(usage.CacheWriteTokens) * rate.cacheWritePerM / 1_000_000
			return CostBreakdown{
				Input:      inCost,
				Output:     outCost,
				CacheRead:  readCost,
				CacheWrite: writeCost,
				Total:      inCost + outCost + readCost + writeCost,
			}, true
		}
	}
	return CostBreakdown{}, false
}

// RunMetadata identifies the execution context of a metered run.
type RunMetadata struct {
	RunID      string
	AttemptID  string
	ThreadID   string
	ProviderID string
	Model      string
}

// RunMetrics is the persisted per-run (or per-attempt) accounting record stored
// in the usage Ledger.
type RunMetrics struct {
	RunID            string        `json:"runId"`
	AttemptID        string        `json:"attemptId,omitempty"`
	ThreadID         string        `json:"threadId"`
	ProviderID       string        `json:"providerId"`
	Model            string        `json:"model"`
	TurnCount         int           `json:"turnCount"`
	RetryCount        int           `json:"retryCount,omitempty"`
	InputTokens       int64         `json:"inputTokens"`
	OutputTokens     int64         `json:"outputTokens"`
	CacheReadTokens  int64         `json:"cacheReadTokens"`
	CacheWriteTokens int64         `json:"cacheWriteTokens"`
	ReasoningTokens   int64         `json:"reasoningTokens,omitempty"`
	TotalTokens       int64         `json:"totalTokens"`
	LastContextTokens int64         `json:"lastContextTokens,omitempty"`
	CostUSD           float64       `json:"costUsd"`
	Cost             CostBreakdown `json:"cost"`
	CostEstimated    bool          `json:"costEstimated,omitempty"`
	TTFTMs           int64         `json:"ttftMs"`
	DurationMs       int64         `json:"durationMs"`
	StartedAt        time.Time     `json:"startedAt"`
	CompletedAt      time.Time     `json:"completedAt"`
}

// TTFT returns the time-to-first-token as a time.Duration.
func (m RunMetrics) TTFT() time.Duration {
	return time.Duration(m.TTFTMs) * time.Millisecond
}

// Duration returns the total run elapsed duration as a time.Duration.
func (m RunMetrics) Duration() time.Duration {
	return time.Duration(m.DurationMs) * time.Millisecond
}

func (m RunMetrics) recordKey() string {
	if m.AttemptID != "" {
		return m.RunID + "\x00" + m.AttemptID
	}
	return m.RunID
}

func normalizeRunMetrics(m RunMetrics) RunMetrics {
	m.RunID = strings.TrimSpace(m.RunID)
	m.AttemptID = strings.TrimSpace(m.AttemptID)
	m.ThreadID = strings.TrimSpace(m.ThreadID)
	m.ProviderID = strings.TrimSpace(m.ProviderID)
	m.Model = strings.TrimSpace(m.Model)
	if m.ProviderID == "" && strings.Contains(m.Model, "/") {
		if prefix, _, ok := strings.Cut(m.Model, "/"); ok {
			m.ProviderID = prefix
		}
	}
	if m.TotalTokens == 0 {
		m.TotalTokens = m.InputTokens + m.OutputTokens + m.CacheReadTokens + m.CacheWriteTokens
	}
	if m.Cost.Total == 0 {
		sum := m.Cost.Input + m.Cost.Output + m.Cost.CacheRead + m.Cost.CacheWrite
		if sum > 0 {
			m.Cost.Total = sum
		} else if m.CostUSD > 0 {
			m.Cost.Total = m.CostUSD
		}
	}
	if m.CostUSD == 0 && m.Cost.Total > 0 {
		m.CostUSD = m.Cost.Total
	}
	if m.CostUSD == 0 && m.Cost.Total == 0 && m.TotalTokens > 0 && m.Model != "" {
		modelRef := m.Model
		if m.ProviderID != "" && !strings.Contains(modelRef, "/") {
			modelRef = m.ProviderID + "/" + modelRef
		}
		if est, ok := EstimateCost(modelRef, TurnUsage{
			InputTokens:      m.InputTokens,
			OutputTokens:     m.OutputTokens,
			CacheReadTokens:  m.CacheReadTokens,
			CacheWriteTokens: m.CacheWriteTokens,
		}); ok {
			m.Cost = est
			m.CostUSD = est.Total
			m.CostEstimated = true
		}
	}
	if m.DurationMs == 0 && !m.StartedAt.IsZero() && !m.CompletedAt.IsZero() && !m.CompletedAt.Before(m.StartedAt) {
		m.DurationMs = m.CompletedAt.Sub(m.StartedAt).Milliseconds()
	}
	if m.CompletedAt.IsZero() && !m.StartedAt.IsZero() && m.DurationMs > 0 {
		m.CompletedAt = m.StartedAt.Add(time.Duration(m.DurationMs) * time.Millisecond)
	}
	if m.StartedAt.IsZero() && !m.CompletedAt.IsZero() {
		m.StartedAt = m.CompletedAt.Add(-time.Duration(m.DurationMs) * time.Millisecond)
	}
	if m.StartedAt.IsZero() && m.CompletedAt.IsZero() {
		now := time.Now().UTC()
		m.StartedAt = now
		m.CompletedAt = now
	}
	return m
}

// RunCollector observes a live provider.Event stream for a single run attempt,
// measuring TTFT, total elapsed duration, and deduplicated multi-turn usage.
type RunCollector struct {
	mu               sync.Mutex
	meta             RunMetadata
	startedAt        time.Time
	firstTokenAt     time.Time
	lastEventAt      time.Time
	turnCount         int
	retryCount        int
	accumulated       TurnUsage
	pendingStreaming  *TurnUsage
	settledTurnSeq    bool
	lastResponseID    string
	lastContextTokens int64
}

// NewRunCollector creates a streaming usage & latency collector for one run/attempt.
func NewRunCollector(meta RunMetadata, startedAt time.Time) *RunCollector {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return &RunCollector{
		meta:      meta,
		startedAt: startedAt.UTC(),
	}
}

// Observe ingests one provider.Event in sequence order.
func (c *RunCollector) Observe(event provider.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ts := event.OccurredAt
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}
	c.lastEventAt = ts

	rawType := rawEventType(event)
	effectiveType := event.Type
	if rawType != "" && (effectiveType == "error" || effectiveType == "") {
		effectiveType = rawType
	}

	// Capture TTFT on the first visible or reasoning token delta.
	if c.firstTokenAt.IsZero() && isFirstTokenSignal(event, rawType) {
		c.firstTokenAt = ts
	}

	switch effectiveType {
	case "auto_retry_start", "summarization_retry_scheduled":
		c.retryCount++
	case "turn_start", "message_start":
		if effectiveType == "turn_start" || isAssistantMessageStart(event.Data) {
			c.settledTurnSeq = false
			c.pendingStreaming = nil
		}
	case "message_update", "text_delta", "thinking_delta":
		if u, ok := ExtractTurnUsage(event.Data); ok {
			c.adoptIdentity(u)
			c.pendingStreaming = &u
		}
	case "message_end":
		if u, ok := ExtractTurnUsage(event.Data); ok {
			c.commitTurn(u)
			c.settledTurnSeq = true
			c.pendingStreaming = nil
		}
	case "turn_end":
		// Pi emits both message_end and turn_end with the same assistant message.
		// Commit on turn_end only when message_end did not already settle this turn.
		if !c.settledTurnSeq {
			if u, ok := ExtractTurnUsage(event.Data); ok {
				c.commitTurn(u)
			}
		}
		c.settledTurnSeq = false
		c.pendingStreaming = nil
	case "compaction_end", "result", "message_delta", "response.completed":
		if u, ok := ExtractTurnUsage(event.Data); ok {
			c.adoptIdentity(u)
			c.accumulated = c.accumulated.Add(u)
			if effectiveType != "compaction_end" {
				c.turnCount++
			}
			c.pendingStreaming = nil
		}
	}
}

func (c *RunCollector) commitTurn(u TurnUsage) {
	if u.ResponseID != "" && u.ResponseID == c.lastResponseID {
		return
	}
	if u.ResponseID != "" {
		c.lastResponseID = u.ResponseID
	}
	c.adoptIdentity(u)
	c.accumulated = c.accumulated.Add(u)
	c.turnCount++
	if turnCtx := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens; turnCtx > 0 {
		c.lastContextTokens = turnCtx
	}
}

func (c *RunCollector) adoptIdentity(u TurnUsage) {
	if c.meta.ProviderID == "" && u.Provider != "" {
		c.meta.ProviderID = u.Provider
	}
	if c.meta.Model == "" {
		c.meta.Model = firstString(u.Model, u.ResponseModel)
	}
}

// Finalize flushes any uncommitted streaming usage (for interrupted streams)
// and produces the final RunMetrics for persistence in Ledger.
func (c *RunCollector) Finalize(completedAt time.Time) RunMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pendingStreaming != nil {
		c.commitTurn(*c.pendingStreaming)
		c.pendingStreaming = nil
	}
	if completedAt.IsZero() {
		if !c.lastEventAt.IsZero() {
			completedAt = c.lastEventAt
		} else {
			completedAt = time.Now().UTC()
		}
	} else {
		completedAt = completedAt.UTC()
	}
	return c.snapshotLocked(completedAt)
}

// Metrics returns a non-destructive snapshot of the collector's metrics so far.
func (c *RunCollector) Metrics(now time.Time) RunMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.snapshotLocked(now.UTC())
}

func (c *RunCollector) snapshotLocked(endTime time.Time) RunMetrics {
	usage := c.accumulated
	turns := c.turnCount
	lastCtx := c.lastContextTokens
	if c.pendingStreaming != nil {
		usage = usage.Add(*c.pendingStreaming)
		turns++
		if pCtx := c.pendingStreaming.InputTokens + c.pendingStreaming.CacheReadTokens + c.pendingStreaming.CacheWriteTokens + c.pendingStreaming.OutputTokens; pCtx > 0 {
			lastCtx = pCtx
		}
	}
	var ttftMs int64
	if !c.firstTokenAt.IsZero() && !c.firstTokenAt.Before(c.startedAt) {
		ttftMs = c.firstTokenAt.Sub(c.startedAt).Milliseconds()
	}
	var durationMs int64
	if !endTime.Before(c.startedAt) {
		durationMs = endTime.Sub(c.startedAt).Milliseconds()
	}
	return normalizeRunMetrics(RunMetrics{
		RunID:             c.meta.RunID,
		AttemptID:         c.meta.AttemptID,
		ThreadID:          c.meta.ThreadID,
		ProviderID:        c.meta.ProviderID,
		Model:             c.meta.Model,
		TurnCount:         turns,
		RetryCount:        c.retryCount,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheReadTokens:   usage.CacheReadTokens,
		CacheWriteTokens:  usage.CacheWriteTokens,
		ReasoningTokens:   usage.ReasoningTokens,
		TotalTokens:       usage.TotalTokens,
		LastContextTokens: lastCtx,
		CostUSD:           usage.Cost.Total,
		Cost:             usage.Cost,
		CostEstimated:    usage.CostEstimated,
		TTFTMs:           ttftMs,
		DurationMs:       durationMs,
		StartedAt:        c.startedAt,
		CompletedAt:      endTime,
	})
}

// UsageSummary aggregates token counts, cache hit efficiency, cost, and latency
// for UI status bars and usage cards.
type UsageSummary struct {
	ThreadID         string        `json:"threadId,omitempty"`
	RunCount         int           `json:"runCount"`
	AttemptCount     int           `json:"attemptCount"`
	TurnCount         int           `json:"turnCount"`
	RetryCount        int           `json:"retryCount,omitempty"`
	InputTokens       int64         `json:"inputTokens"`
	OutputTokens     int64         `json:"outputTokens"`
	CacheReadTokens  int64         `json:"cacheReadTokens"`
	CacheWriteTokens int64         `json:"cacheWriteTokens"`
	CacheHitTokens   int64         `json:"cacheHitTokens"`
	ReasoningTokens  int64         `json:"reasoningTokens,omitempty"`
	TotalTokens      int64         `json:"totalTokens"`
	CacheHitRate     float64       `json:"cacheHitRate"`
	TotalCostUSD     float64       `json:"totalCostUsd"`
	Cost             CostBreakdown `json:"cost"`
	AvgTTFTMs        int64         `json:"avgTtftMs"`
	TotalDurationMs  int64         `json:"totalDurationMs"`
	LastUpdatedAt    time.Time     `json:"lastUpdatedAt,omitempty"`
}

// ModelUsage reports aggregated usage metrics for one ProviderID + Model pair.
type ModelUsage struct {
	ProviderID string `json:"providerId"`
	Model      string `json:"model"`
	UsageSummary
}

type persistedLedger struct {
	Version   int          `json:"version"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Records   []RunMetrics `json:"records"`
}

// Ledger is a thread-safe, atomically persisted store of per-run token, cost,
// and latency metrics backed by `<dataDir>/usage-ledger.json`.
type Ledger struct {
	mu      sync.RWMutex
	dataDir string
	path    string
	order   []string
	byKey   map[string]RunMetrics
}

// NewLedger creates or loads a usage Ledger. When dataDir is empty, Ledger
// operates purely in memory; otherwise it loads `<dataDir>/usage-ledger.json`
// if present and persists subsequent Record updates atomically.
func NewLedger(dataDir string) (*Ledger, error) {
	l := &Ledger{
		dataDir: strings.TrimSpace(dataDir),
		byKey:   make(map[string]RunMetrics),
	}
	if l.dataDir != "" {
		abs, err := filepath.Abs(l.dataDir)
		if err != nil {
			return nil, fmt.Errorf("resolve telemetry data dir: %w", err)
		}
		l.dataDir = abs
		l.path = filepath.Join(abs, ledgerFileName)
		if err := l.Reload(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Path returns the backing JSON file path, or empty string for an in-memory Ledger.
func (l *Ledger) Path() string {
	return l.path
}

// Reload reads the persisted ledger file from disk if configured and present.
func (l *Ledger) Reload() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read usage ledger: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var persisted persistedLedger
	if err := json.Unmarshal(data, &persisted); err != nil {
		return fmt.Errorf("decode usage ledger: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = l.order[:0]
	l.byKey = make(map[string]RunMetrics, len(persisted.Records))
	for _, rec := range persisted.Records {
		norm := normalizeRunMetrics(rec)
		key := norm.recordKey()
		if key == "" {
			continue
		}
		if _, exists := l.byKey[key]; !exists {
			l.order = append(l.order, key)
		}
		l.byKey[key] = norm
	}
	return nil
}

// Record upserts a RunMetrics entry (idempotent by RunID + AttemptID) and
// atomically persists the updated ledger when backed by a data directory.
func (l *Ledger) Record(metrics RunMetrics) error {
	norm := normalizeRunMetrics(metrics)
	if norm.RunID == "" {
		return errors.New("telemetry: RunMetrics.RunID is required")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	key := norm.recordKey()
	if _, exists := l.byKey[key]; !exists {
		l.order = append(l.order, key)
	}
	l.byKey[key] = norm

	if l.path == "" {
		return nil
	}
	return l.saveLocked()
}

// Save flushes the current in-memory state to `<dataDir>/usage-ledger.json`.
func (l *Ledger) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		return nil
	}
	return l.saveLocked()
}

func (l *Ledger) saveLocked() error {
	if err := os.MkdirAll(l.dataDir, 0o700); err != nil {
		return fmt.Errorf("create telemetry data dir: %w", err)
	}
	records := make([]RunMetrics, 0, len(l.order))
	for _, key := range l.order {
		records = append(records, l.byKey[key])
	}
	payload := persistedLedger{
		Version:   1,
		UpdatedAt: time.Now().UTC(),
		Records:   records,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode usage ledger: %w", err)
	}
	encoded = append(encoded, '\n')

	tmpFile, err := os.CreateTemp(l.dataDir, ".usage-ledger-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp usage ledger: %w", err)
	}
	tmpName := tmpFile.Name()
	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmpFile.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp usage ledger: %w", err)
	}
	if _, err := tmpFile.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("write temp usage ledger: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp usage ledger: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp usage ledger: %w", err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit usage ledger: %w", err)
	}
	return nil
}

// Records returns a snapshot copy of all recorded RunMetrics in insertion order.
func (l *Ledger) Records() []RunMetrics {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]RunMetrics, 0, len(l.order))
	for _, key := range l.order {
		out = append(out, l.byKey[key])
	}
	return out
}

// GetRun returns aggregated metrics for a single RunID across its attempts.
func (l *Ledger) GetRun(runID string) (RunMetrics, bool) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunMetrics{}, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()

	var (
		found bool
		agg   RunMetrics
	)
	for _, key := range l.order {
		rec := l.byKey[key]
		if rec.RunID != runID {
			continue
		}
		if !found {
			agg = rec
			found = true
			continue
		}
		agg.TurnCount += rec.TurnCount
		agg.RetryCount += rec.RetryCount
		agg.InputTokens += rec.InputTokens
		agg.OutputTokens += rec.OutputTokens
		agg.CacheReadTokens += rec.CacheReadTokens
		agg.CacheWriteTokens += rec.CacheWriteTokens
		agg.ReasoningTokens += rec.ReasoningTokens
		agg.TotalTokens += rec.TotalTokens
		if rec.LastContextTokens > 0 {
			agg.LastContextTokens = rec.LastContextTokens
		}
		agg.Cost = agg.Cost.Add(rec.Cost)
		agg.CostUSD = agg.Cost.Total
		agg.CostEstimated = agg.CostEstimated || rec.CostEstimated
		agg.DurationMs += rec.DurationMs
		if agg.TTFTMs == 0 || (rec.TTFTMs > 0 && rec.StartedAt.Before(agg.StartedAt)) {
			agg.TTFTMs = rec.TTFTMs
		}
		if !rec.StartedAt.IsZero() && (agg.StartedAt.IsZero() || rec.StartedAt.Before(agg.StartedAt)) {
			agg.StartedAt = rec.StartedAt
		}
		if rec.CompletedAt.After(agg.CompletedAt) {
			agg.CompletedAt = rec.CompletedAt
		}
	}
	return agg, found
}

// ThreadSummary aggregates all runs belonging to threadID.
func (l *Ledger) ThreadSummary(threadID string) UsageSummary {
	threadID = strings.TrimSpace(threadID)
	l.mu.RLock()
	defer l.mu.RUnlock()

	var matched []RunMetrics
	for _, key := range l.order {
		rec := l.byKey[key]
		if rec.ThreadID == threadID {
			matched = append(matched, rec)
		}
	}
	summary := summarizeRecords(matched)
	summary.ThreadID = threadID
	return summary
}

// ModelBreakdown aggregates usage grouped by (ProviderID, Model) for all runs
// whose timestamp is at or after `since` (pass zero time.Time for all history).
// Results are deterministically ordered by descending cost, then descending tokens.
func (l *Ledger) ModelBreakdown(since time.Time) []ModelUsage {
	l.mu.RLock()
	defer l.mu.RUnlock()

	type modelKey struct {
		provider string
		model    string
	}
	grouped := make(map[modelKey][]RunMetrics)
	for _, key := range l.order {
		rec := l.byKey[key]
		ts := rec.CompletedAt
		if ts.IsZero() {
			ts = rec.StartedAt
		}
		if !since.IsZero() && ts.Before(since) {
			continue
		}
		mk := modelKey{provider: rec.ProviderID, model: rec.Model}
		grouped[mk] = append(grouped[mk], rec)
	}

	out := make([]ModelUsage, 0, len(grouped))
	for mk, records := range grouped {
		out = append(out, ModelUsage{
			ProviderID:   mk.provider,
			Model:        mk.model,
			UsageSummary: summarizeRecords(records),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].TotalCostUSD-out[j].TotalCostUSD) > 1e-12 {
			return out[i].TotalCostUSD > out[j].TotalCostUSD
		}
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// GlobalTotals aggregates usage across every run recorded in the Ledger.
func (l *Ledger) GlobalTotals() UsageSummary {
	l.mu.RLock()
	defer l.mu.RUnlock()

	records := make([]RunMetrics, 0, len(l.order))
	for _, key := range l.order {
		records = append(records, l.byKey[key])
	}
	return summarizeRecords(records)
}

func summarizeRecords(records []RunMetrics) UsageSummary {
	if len(records) == 0 {
		return UsageSummary{}
	}
	var (
		sum       UsageSummary
		seenRuns  = make(map[string]struct{}, len(records))
		ttftSum   int64
		ttftCount int64
	)
	for _, rec := range records {
		sum.AttemptCount++
		if rec.RunID != "" {
			if _, ok := seenRuns[rec.RunID]; !ok {
				seenRuns[rec.RunID] = struct{}{}
				sum.RunCount++
			}
		} else {
			sum.RunCount++
		}
		turns := rec.TurnCount
		if turns == 0 && rec.TotalTokens > 0 {
			turns = 1
		}
		sum.TurnCount += turns
		sum.RetryCount += rec.RetryCount
		sum.InputTokens += rec.InputTokens
		sum.OutputTokens += rec.OutputTokens
		sum.CacheReadTokens += rec.CacheReadTokens
		sum.CacheWriteTokens += rec.CacheWriteTokens
		sum.CacheHitTokens += rec.CacheReadTokens
		sum.ReasoningTokens += rec.ReasoningTokens
		sum.TotalTokens += rec.TotalTokens
		sum.Cost = sum.Cost.Add(rec.Cost)
		sum.TotalDurationMs += rec.DurationMs
		if rec.TTFTMs > 0 {
			ttftSum += rec.TTFTMs
			ttftCount++
		}
		ts := rec.CompletedAt
		if ts.IsZero() {
			ts = rec.StartedAt
		}
		if ts.After(sum.LastUpdatedAt) {
			sum.LastUpdatedAt = ts
		}
	}
	sum.TotalCostUSD = sum.Cost.Total
	if ttftCount > 0 {
		sum.AvgTTFTMs = ttftSum / ttftCount
	}
	promptTotal := sum.InputTokens + sum.CacheReadTokens + sum.CacheWriteTokens
	if promptTotal > 0 && sum.CacheReadTokens > 0 {
		rate := float64(sum.CacheReadTokens) / float64(promptTotal)
		if rate > 1 {
			rate = 1
		}
		sum.CacheHitRate = rate
	}
	return sum
}

func isFirstTokenSignal(event provider.Event, rawType string) bool {
	if event.Delta != "" {
		return true
	}
	switch event.Type {
	case "text_delta", "thinking_delta":
		return true
	}
	if rawType == "message_update" && len(event.Data) > 0 {
		var msg struct {
			AssistantMessageEvent struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if json.Unmarshal(event.Data, &msg) == nil {
			switch msg.AssistantMessageEvent.Type {
			case "text_delta", "thinking_delta", "toolcall_start", "toolcall_delta":
				return true
			}
		}
	}
	return false
}

func isAssistantMessageStart(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var msg struct {
		Message struct {
			Role string `json:"role"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return true
	}
	return msg.Message.Role == "" || strings.EqualFold(msg.Message.Role, "assistant")
}

func rawEventType(event provider.Event) string {
	if len(event.Data) == 0 {
		return ""
	}
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(event.Data, &env) != nil {
		return ""
	}
	return env.Type
}

func nestedCachedTokens(w *rawUsageWire) *int64 {
	if w.PromptTokensDetails != nil && w.PromptTokensDetails.CachedTokens != nil {
		return w.PromptTokensDetails.CachedTokens
	}
	if w.InputTokensDetails != nil && w.InputTokensDetails.CachedTokens != nil {
		return w.InputTokensDetails.CachedTokens
	}
	return nil
}

func nestedReasoningTokens(w *rawUsageWire) *int64 {
	if w.CompletionTokensDetails != nil && w.CompletionTokensDetails.ReasoningTokens != nil {
		return w.CompletionTokensDetails.ReasoningTokens
	}
	if w.OutputTokensDetails != nil && w.OutputTokensDetails.ReasoningTokens != nil {
		return w.OutputTokensDetails.ReasoningTokens
	}
	return nil
}

func nestedResultCost(env rawEnvelopeWire) *float64 {
	if env.Result != nil {
		return env.Result.TotalCostUSD
	}
	return nil
}

func firstInt64(ptrs ...*int64) int64 {
	for _, p := range ptrs {
		if p != nil {
			return *p
		}
	}
	return 0
}

func firstInt64Ptr(ptrs ...*int64) *int64 {
	for _, p := range ptrs {
		if p != nil {
			return p
		}
	}
	return nil
}

func firstFloat64(ptrs ...*float64) float64 {
	for _, p := range ptrs {
		if p != nil {
			return *p
		}
	}
	return 0
}

func firstFloat64Ptr(ptrs ...*float64) *float64 {
	for _, p := range ptrs {
		if p != nil {
			return p
		}
	}
	return nil
}

func derefFloat64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func firstString(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
