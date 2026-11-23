package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// FailureKind categorizes provider errors for retry and model-fallback policy.
type FailureKind string

const (
	RateLimited     FailureKind = "rate_limited"
	QuotaExhausted  FailureKind = "quota_exhausted"
	Overloaded      FailureKind = "overloaded"
	ContextOverflow FailureKind = "context_overflow"
	PermanentError  FailureKind = "permanent"
)

// RecoveryAction describes the recommended automated recovery step.
type RecoveryAction string

const (
	ActionRetryBackoff   RecoveryAction = "retry_with_backoff"
	ActionSwitchFallback RecoveryAction = "switch_fallback_model"
	ActionCompactContext RecoveryAction = "compact_context"
	ActionAbort          RecoveryAction = "abort"
)

const (
	DefaultBaseBackoff   = 2 * time.Second
	DefaultMaxBackoff    = 60 * time.Second
	DefaultMaxJitter     = 2 * time.Second
	DefaultMaxRetryAfter = 5 * time.Minute
	DefaultMaxRetries    = 3
)

// LimitDiagnosis is the structured classification of a provider failure.
type LimitDiagnosis struct {
	Kind           FailureKind    `json:"kind"`
	StatusCode     int            `json:"statusCode,omitempty"`
	Retryable      bool           `json:"retryable"`
	ShouldFallback bool           `json:"shouldFallback"`
	Action         RecoveryAction `json:"action"`
	RetryAfter     time.Duration  `json:"retryAfter,omitempty"`
	ResetAt        time.Time      `json:"resetAt,omitempty"`
	ProviderCode   string         `json:"providerCode,omitempty"`
	Summary        string         `json:"summary"`
}

var (
	statusPrefixRe = regexp.MustCompile(`(?i)^\s*(?:http(?:/\d(?:\.\d)?)?\s+)?(400|401|402|403|404|408|409|413|422|429|500|502|503|504|529)\b`)
	statusLabelRe  = regexp.MustCompile(`(?i)\b(?:http(?:/\d(?:\.\d)?)?|status(?:\s+code)?|error(?:\s+code)?|returned|got)\s*[:=]?\s*(400|401|402|403|404|408|409|413|422|429|500|502|503|504|529)\b`)
	statusPhraseRe = regexp.MustCompile(`(?i)\b(402|429|502|503|504|529)\s+(?:payment required|too many requests|bad gateway|service unavailable|gateway timeout|overloaded)\b`)

	retryAfterHeaderInTextRe = regexp.MustCompile(`(?i)retry-after\s*[:=]\s*(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|sec|seconds?|m|min|minutes?|h|hr|hours?)?`)
	compoundDurationPhraseRe = regexp.MustCompile(`(?i)(?:retry\s+after|try\s+again\s+in|retry\s+in|resets?\s+in|wait(?:ing)?(?:\s+for)?)\s+((?:\d+h)?(?:\d+m)?\d+(?:\.\d+)?(?:ms|s|m|h)\b)`)
	unitDurationPhraseRe     = regexp.MustCompile(`(?i)(?:retry\s+after|try\s+again\s+in|retry\s+in|resets?\s+in|wait(?:ing)?(?:\s+for)?)\s+(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|sec|seconds?|m|min|minutes?|h|hr|hours?)\b`)
	zhDurationPhraseRe       = regexp.MustCompile(`(?:请在|等待|请等待|重试间隔[:：\s]*)\s*(\d+(?:\.\d+)?)\s*(毫秒|秒钟?|分钟|小时)`)
)

// ClassifyError inspects an error string or JSON error payload and classifies it
// into RateLimited, QuotaExhausted, Overloaded, ContextOverflow, or PermanentError.
func ClassifyError(errText string) LimitDiagnosis {
	return ClassifyErrorWithHeaders(errText, nil, time.Now().UTC())
}

// ClassifyErrorWithHeaders classifies an error using both response headers
// (such as Retry-After / X-RateLimit-Reset-*) and the error body/message.
func ClassifyErrorWithHeaders(errText string, headers map[string]string, now time.Time) LimitDiagnosis {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	trimmed := strings.TrimSpace(errText)
	statusCode, providerCode, jsonMessage, jsonRetryAfter := inspectJSONError(trimmed)
	if statusCode == 0 {
		statusCode = extractStatusCode(trimmed)
	}

	combined := trimmed
	if jsonMessage != "" && !strings.Contains(combined, jsonMessage) {
		combined = combined + " " + jsonMessage
	}
	lower := strings.ToLower(combined)
	codeLower := strings.ToLower(providerCode)

	retryAfter := extractRetryWait(combined, headers, jsonRetryAfter, now)
	var resetAt time.Time
	if retryAfter > 0 {
		resetAt = now.Add(retryAfter)
	}

	// 1. ContextOverflow check first (cannot succeed by blind retry on same prompt).
	if isContextOverflow(statusCode, codeLower, lower) {
		return LimitDiagnosis{
			Kind:           ContextOverflow,
			StatusCode:     statusCode,
			Retryable:      false,
			ShouldFallback: false,
			Action:         ActionCompactContext,
			ProviderCode:   providerCode,
			Summary:        summarizeErrorText(firstString(jsonMessage, trimmed), "context window limit exceeded"),
		}
	}

	// 2. QuotaExhausted / Billing / Credits check BEFORE generic 429 rate-limit,
	// because OpenAI/gateways often return HTTP 429 for `insufficient_quota` or `credits_required`.
	if isQuotaExhausted(statusCode, codeLower, lower) {
		return LimitDiagnosis{
			Kind:           QuotaExhausted,
			StatusCode:     statusCode,
			Retryable:      false,
			ShouldFallback: true,
			Action:         ActionSwitchFallback,
			RetryAfter:     retryAfter,
			ResetAt:        resetAt,
			ProviderCode:   providerCode,
			Summary:        summarizeErrorText(firstString(jsonMessage, trimmed), "model quota or account credits exhausted"),
		}
	}

	// 3. Overloaded (529 / 503 / 502 / 504 / overloaded_error).
	if isOverloaded(statusCode, codeLower, lower) {
		return LimitDiagnosis{
			Kind:           Overloaded,
			StatusCode:     statusCode,
			Retryable:      true,
			ShouldFallback: retryAfter > DefaultMaxBackoff,
			Action:         ActionRetryBackoff,
			RetryAfter:     retryAfter,
			ResetAt:        resetAt,
			ProviderCode:   providerCode,
			Summary:        summarizeErrorText(firstString(jsonMessage, trimmed), "provider service temporarily overloaded"),
		}
	}

	// 4. RateLimited (429 / Too Many Requests / RPM / TPM).
	if isRateLimited(statusCode, codeLower, lower, headers) {
		longWait := retryAfter > 2*DefaultMaxBackoff
		action := ActionRetryBackoff
		if longWait {
			action = ActionSwitchFallback
		}
		return LimitDiagnosis{
			Kind:           RateLimited,
			StatusCode:     statusCode,
			Retryable:      !longWait,
			ShouldFallback: longWait,
			Action:         action,
			RetryAfter:     retryAfter,
			ResetAt:        resetAt,
			ProviderCode:   providerCode,
			Summary:        summarizeErrorText(firstString(jsonMessage, trimmed), "provider rate limit reached"),
		}
	}

	// 5. Ordinary permanent / non-retryable error.
	return LimitDiagnosis{
		Kind:           PermanentError,
		StatusCode:     statusCode,
		Retryable:      false,
		ShouldFallback: false,
		Action:         ActionAbort,
		ProviderCode:   providerCode,
		Summary:        summarizeErrorText(firstString(jsonMessage, trimmed), "non-retryable provider error"),
	}
}

func isContextOverflow(status int, codeLower, lower string) bool {
	for _, token := range []string{
		"context_length_exceeded",
		"context_overflow",
		"context_window_exceeded",
		"string_above_max_length",
		"prompt_too_long",
	} {
		if strings.Contains(codeLower, token) || strings.Contains(lower, token) {
			return true
		}
	}
	for _, phrase := range []string{
		"prompt is too long",
		"input is too long",
		"maximum context length",
		"exceeds the model's maximum context",
		"exceeds maximum context",
		"context window",
		"context length",
		"token limit exceeded",
		"too many input tokens",
		"上下文过长",
		"超出最大上下文",
	} {
		if strings.Contains(lower, phrase) {
			if phrase == "context window" || phrase == "context length" {
				if strings.Contains(lower, "exceed") || strings.Contains(lower, "too long") || strings.Contains(lower, "too large") || strings.Contains(lower, "overflow") || strings.Contains(lower, "limit") {
					return true
				}
				continue
			}
			return true
		}
	}
	return status == 413 && strings.Contains(lower, "token")
}

func isQuotaExhausted(status int, codeLower, lower string) bool {
	if status == 402 {
		return true
	}
	for _, code := range []string{
		"insufficient_quota",
		"credits_required",
		"credit_balance_too_low",
		"billing_hard_limit_reached",
		"billing_not_active",
		"out_of_credits",
		"payment_required",
		"quota_exhausted",
		"spend_limit_reached",
	} {
		if strings.Contains(codeLower, code) || strings.Contains(lower, code) {
			return true
		}
	}
	for _, phrase := range []string{
		"exceeded your current quota",
		"exhausted your quota",
		"credit balance is too low",
		"out of credits",
		"insufficient credits",
		"no remaining credits",
		"purchase more credits",
		"check your plan and billing",
		"billing details",
		"billing limit",
		"account billing",
		"spend limit",
		"monthly usage limit",
		"monthly limit reached",
		"daily quota",
		"额度已用尽",
		"配额已耗尽",
		"配额不足",
		"余额不足",
		"欠费",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	// Distinguish transient RPM/TPM "quota metric ... per minute" from real quota exhaustion.
	if strings.Contains(lower, "quota") {
		isShortWindowRate := strings.Contains(lower, "per minute") ||
			strings.Contains(lower, "per second") ||
			strings.Contains(lower, "per_minute") ||
			strings.Contains(lower, "requests per min") ||
			strings.Contains(lower, "rpm") ||
			strings.Contains(lower, "tpm")
		if !isShortWindowRate {
			return true
		}
	}
	if strings.Contains(lower, "billing") {
		return true
	}
	return false
}

func isOverloaded(status int, codeLower, lower string) bool {
	switch status {
	case 529, 503, 502, 504:
		return true
	}
	for _, marker := range []string{
		"overloaded_error",
		"server_is_overloaded",
		"service_unavailable",
		"temporarily overloaded",
		"currently overloaded",
		"overloaded",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"upstream connect error",
		"at capacity",
		"no capacity available",
		"服务过载",
		"服务繁忙",
	} {
		if strings.Contains(codeLower, marker) || strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isRateLimited(status int, codeLower, lower string, headers map[string]string) bool {
	if status == 429 {
		return true
	}
	for _, marker := range []string{
		"rate_limit_error",
		"rate_limit_exceeded",
		"rate_limited",
		"too_many_requests",
		"too many requests",
		"rate limit",
		"ratelimit",
		"requests per minute",
		"tokens per minute",
		"throttled",
		"throttling",
		"请求过于频繁",
		"触发限流",
	} {
		if strings.Contains(codeLower, marker) || strings.Contains(lower, marker) {
			return true
		}
	}
	if lookupHeader(headers, "retry-after") != "" && (status == 0 || status >= 400) {
		if strings.Contains(lower, "limit") || strings.Contains(lower, "retry") {
			return true
		}
	}
	return false
}

// ParseRetryAfter parses an HTTP Retry-After value (delta-seconds, duration
// string like "6m12s", or RFC1123 HTTP-date) relative to `now`.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Plain number -> seconds (or Unix epoch if >= 1_700_000_000).
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		if secs >= 1_700_000_000 {
			until := time.Unix(int64(secs), 0).Sub(now)
			if until <= 0 {
				return 0, false
			}
			return until, true
		}
		return time.Duration(secs * float64(time.Second)), true
	}

	// Go-style compound duration ("6m0s", "450ms", "1.5s").
	if d, err := time.ParseDuration(strings.ToLower(v)); err == nil && d > 0 {
		return d, true
	}

	// Number + word unit ("20 seconds", "2 minutes").
	if m := regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|sec|seconds?|m|min|minutes?|h|hr|hours?)$`).FindStringSubmatch(v); len(m) == 3 {
		if d, ok := durationFromNumberUnit(m[1], m[2]); ok {
			return d, true
		}
	}

	// Standard HTTP-date header format.
	if ts, err := http.ParseTime(v); err == nil {
		until := ts.Sub(now)
		if until <= 0 {
			return 0, false
		}
		return until, true
	}
	// RFC3339 timestamp fallback.
	if ts, err := time.Parse(time.RFC3339, v); err == nil {
		until := ts.Sub(now)
		if until <= 0 {
			return 0, false
		}
		return until, true
	}

	return 0, false
}

func extractRetryWait(text string, headers map[string]string, jsonWait time.Duration, now time.Time) time.Duration {
	var best time.Duration
	consider := func(d time.Duration) {
		if d > best {
			best = d
		}
	}

	if v := lookupHeader(headers, "retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && ms > 0 {
			consider(time.Duration(ms * float64(time.Millisecond)))
		}
	}
	if v := lookupHeader(headers, "retry-after"); v != "" {
		if d, ok := ParseRetryAfter(v, now); ok {
			consider(d)
		}
	}
	for _, key := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "x-ratelimit-reset"} {
		if v := lookupHeader(headers, key); v != "" {
			if d, ok := ParseRetryAfter(v, now); ok {
				consider(d)
			}
		}
	}

	consider(jsonWait)

	if m := retryAfterHeaderInTextRe.FindStringSubmatch(text); len(m) == 3 {
		unit := m[2]
		if unit == "" {
			unit = "s"
		}
		if d, ok := durationFromNumberUnit(m[1], unit); ok {
			consider(d)
		}
	}
	for _, m := range compoundDurationPhraseRe.FindAllStringSubmatch(text, -1) {
		if len(m) == 2 {
			if d, err := time.ParseDuration(strings.ToLower(m[1])); err == nil && d > 0 {
				consider(d)
			}
		}
	}
	for _, m := range unitDurationPhraseRe.FindAllStringSubmatch(text, -1) {
		if len(m) == 3 {
			if d, ok := durationFromNumberUnit(m[1], m[2]); ok {
				consider(d)
			}
		}
	}
	if m := zhDurationPhraseRe.FindStringSubmatch(text); len(m) == 3 {
		if d, ok := durationFromNumberUnit(m[1], m[2]); ok {
			consider(d)
		}
	}

	return best
}

func durationFromNumberUnit(numStr, unitStr string) (time.Duration, bool) {
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil || val <= 0 {
		return 0, false
	}
	var mult time.Duration
	switch strings.ToLower(strings.TrimSpace(unitStr)) {
	case "ms", "millisecond", "milliseconds", "毫秒":
		mult = time.Millisecond
	case "s", "sec", "second", "seconds", "秒", "秒钟":
		mult = time.Second
	case "m", "min", "minute", "minutes", "分钟":
		mult = time.Minute
	case "h", "hr", "hour", "hours", "小时":
		mult = time.Hour
	default:
		return 0, false
	}
	return time.Duration(val * float64(mult)), true
}

// BackoffConfig customizes exponential backoff and bounded jitter calculation.
type BackoffConfig struct {
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	MaxJitter     time.Duration
	MaxRetryAfter time.Duration
	// Rand returns a float64 in [0.0, 1.0). Nil uses math/rand/v2.Float64.
	Rand func() float64
}

func (c BackoffConfig) normalized() BackoffConfig {
	if c.BaseDelay <= 0 {
		c.BaseDelay = DefaultBaseBackoff
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = DefaultMaxBackoff
	}
	if c.MaxJitter <= 0 {
		c.MaxJitter = DefaultMaxJitter
	}
	if c.MaxRetryAfter <= 0 {
		c.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	return c
}

// Compute calculates the backoff wait duration for a 1-indexed retry attempt,
// honoring server-supplied retryAfter while enforcing upper delay and jitter bounds.
func (c BackoffConfig) Compute(attempt int, retryAfter time.Duration) time.Duration {
	cfg := c.normalized()
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 20 {
		shift = 20
	}

	exp := cfg.BaseDelay * time.Duration(1<<shift)
	if exp <= 0 || exp > cfg.MaxDelay {
		exp = cfg.MaxDelay
	}

	r := cfg.Rand()
	if r < 0 {
		r = 0
	} else if r > 1 {
		r = 1
	}

	// When the server explicitly specified Retry-After, never retry earlier than
	// the requested floor (up to MaxRetryAfter), plus bounded positive jitter.
	if retryAfter > 0 {
		floor := retryAfter
		if floor > cfg.MaxRetryAfter {
			floor = cfg.MaxRetryAfter
		}
		if exp > floor && floor < cfg.MaxDelay {
			floor = exp
		}
		jitterCap := min(cfg.MaxJitter, floor/5)
		jitter := time.Duration(float64(jitterCap) * r)
		total := floor + jitter
		if total > cfg.MaxRetryAfter {
			total = cfg.MaxRetryAfter
		}
		return total
	}

	jitterCap := min(cfg.MaxJitter, exp/4)
	if exp+jitterCap <= cfg.MaxDelay {
		return exp + time.Duration(float64(jitterCap)*r)
	}
	// At the ceiling, keep jitter bounded below MaxDelay so total wait never exceeds MaxDelay.
	ceilingJitter := min(cfg.MaxJitter, cfg.MaxDelay/4)
	base := cfg.MaxDelay - ceilingJitter
	return base + time.Duration(float64(ceilingJitter)*r)
}

// NextBackoff computes exponential backoff with capped jitter using default bounds.
func NextBackoff(attempt int, retryAfter time.Duration) time.Duration {
	return (BackoffConfig{}).Compute(attempt, retryAfter)
}

// ModelTier represents capability/cost tiers used for peer-grade fallback matching.
type ModelTier int

const (
	TierBalanced ModelTier = iota
	TierFlagship
	TierFast
)

func (t ModelTier) String() string {
	switch t {
	case TierFlagship:
		return "flagship"
	case TierFast:
		return "fast"
	default:
		return "balanced"
	}
}

// InferModelTier classifies a provider/model identifier into flagship, balanced, or fast
// using token-boundary matching (so "gemini-2.5-pro" never false-matches "mini").
func InferModelTier(model string) ModelTier {
	lower := strings.ToLower(strings.TrimSpace(model))
	if _, bare, ok := strings.Cut(lower, "/"); ok {
		lower = bare
	}
	tokens := splitModelTokens(lower)
	hasToken := func(targets ...string) bool {
		for _, tok := range tokens {
			for _, t := range targets {
				if tok == t {
					return true
				}
			}
		}
		return false
	}

	if hasToken("haiku", "flash", "mini", "nano", "lite", "small", "instant", "turbo") {
		return TierFast
	}
	if hasToken("opus", "pro", "o1", "o3", "r1", "reasoner", "ultra", "max") ||
		strings.HasPrefix(lower, "gpt-5") {
		return TierFlagship
	}
	return TierBalanced
}

func splitModelTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == ' '
	})
}

// RecoveryQuery supplies context to FallbackAdvisor.Advise.
type RecoveryQuery struct {
	PrimaryModel string
	Attempt      int
	MaxRetries   int
	ErrorText    string
	Headers      map[string]string
	TriedModels  []string
	Now          time.Time
}

// RecoveryDecision is the concrete action plan returned by FallbackAdvisor.Advise.
type RecoveryDecision struct {
	Diagnosis          LimitDiagnosis `json:"diagnosis"`
	Action             RecoveryAction `json:"action"`
	Wait               time.Duration  `json:"wait,omitempty"`
	FallbackModel      string         `json:"fallbackModel,omitempty"`
	FallbackCandidates []string       `json:"fallbackCandidates,omitempty"`
	Reason             string         `json:"reason"`
}

type cooldownEntry struct {
	kind  FailureKind
	until time.Time
}

// FallbackAdvisor combines configured fallback tables, live model catalogs, and
// per-model cooldown tracking to recommend peer-tier fallback models without
// endlessly retrying exhausted paid quotas.
type FallbackAdvisor struct {
	mu                sync.RWMutex
	routes            map[string][]string
	catalog           []api.ModelInfo
	catalogSet        map[string]api.ModelInfo
	cooldowns         map[string]cooldownEntry
	backoff           BackoffConfig
	maxRetries        int
	rateLimitCooldown time.Duration
	quotaCooldown     time.Duration
}

// FallbackOption configures a FallbackAdvisor.
type FallbackOption func(*FallbackAdvisor)

// WithFallbackMap registers explicit primary -> ordered fallback model routes.
func WithFallbackMap(routes map[string][]string) FallbackOption {
	return func(a *FallbackAdvisor) {
		a.mergeRoutesLocked(routes)
	}
}

// WithCatalog registers available candidate models for validation and tier-based fallback.
func WithCatalog(models []api.ModelInfo) FallbackOption {
	return func(a *FallbackAdvisor) {
		a.setCatalogLocked(models)
	}
}

// WithMaxRetries sets the transient retry cap before escalating 429/529 to a fallback model.
func WithMaxRetries(maxRetries int) FallbackOption {
	return func(a *FallbackAdvisor) {
		if maxRetries >= 0 {
			a.maxRetries = maxRetries
		}
	}
}

// WithBackoff customizes the backoff calculator used by Advise.
func WithBackoff(cfg BackoffConfig) FallbackOption {
	return func(a *FallbackAdvisor) {
		a.backoff = cfg
	}
}

// WithCooldowns sets temporary cooldown windows for rate-limited and quota-exhausted models.
func WithCooldowns(rateLimitCooldown, quotaCooldown time.Duration) FallbackOption {
	return func(a *FallbackAdvisor) {
		if rateLimitCooldown > 0 {
			a.rateLimitCooldown = rateLimitCooldown
		}
		if quotaCooldown > 0 {
			a.quotaCooldown = quotaCooldown
		}
	}
}

// NewFallbackAdvisor constructs a thread-safe FallbackAdvisor.
func NewFallbackAdvisor(opts ...FallbackOption) *FallbackAdvisor {
	a := &FallbackAdvisor{
		routes:            make(map[string][]string),
		catalogSet:        make(map[string]api.ModelInfo),
		cooldowns:         make(map[string]cooldownEntry),
		maxRetries:        DefaultMaxRetries,
		rateLimitCooldown: 60 * time.Second,
		quotaCooldown:     30 * time.Minute,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a
}

// ParseFallbackTable parses a fallback mapping from JSON (`{"modelA": ["modelB"]}`)
// or Markdown table / arrow lines (`| primary | fallback1, fallback2 |` or `modelA -> modelB, modelC`).
func ParseFallbackTable(content string) (map[string][]string, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return map[string][]string{}, nil
	}

	if strings.HasPrefix(trimmed, "{") {
		var direct map[string][]string
		if err := json.Unmarshal([]byte(trimmed), &direct); err == nil {
			return normalizeRoutesMap(direct), nil
		}
		var wrapped struct {
			Fallbacks map[string][]string `json:"fallbacks"`
			Routes    map[string][]string `json:"routes"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapped); err != nil {
			return nil, fmt.Errorf("parse fallback JSON: %w", err)
		}
		if len(wrapped.Fallbacks) > 0 {
			return normalizeRoutesMap(wrapped.Fallbacks), nil
		}
		return normalizeRoutesMap(wrapped.Routes), nil
	}

	routes := make(map[string][]string)
	for _, rawLine := range strings.Split(trimmed, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		// Markdown table row: | primary | fallback1, fallback2 | ... |
		if strings.HasPrefix(line, "|") && strings.Contains(line[1:], "|") {
			cells := strings.Split(strings.Trim(line, "|"), "|")
			if len(cells) < 2 {
				continue
			}
			left := cleanModelCell(cells[0])
			right := cleanModelCell(cells[1])
			if left == "" || right == "" || isMarkdownTableHeaderOrSep(left, right) {
				continue
			}
			targets := splitModelTargets(right)
			if len(targets) > 0 {
				routes[left] = appendUnique(routes[left], targets...)
			}
			continue
		}
		// Arrow or colon mapping: primary -> fallback1, fallback2
		for _, sep := range []string{"->", "=>", ":"} {
			if lhs, rhs, ok := strings.Cut(line, sep); ok {
				left := cleanModelCell(strings.TrimLeft(lhs, "-*• "))
				right := cleanModelCell(rhs)
				if left != "" && right != "" && !strings.ContainsAny(left, " \t") {
					targets := splitModelTargets(right)
					if len(targets) > 0 {
						routes[left] = appendUnique(routes[left], targets...)
						break
					}
				}
			}
		}
	}
	return normalizeRoutesMap(routes), nil
}

// LoadFallbackFile reads and parses a JSON or Markdown fallback mapping file.
func LoadFallbackFile(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseFallbackTable(string(data))
}

// LoadMappingText parses and merges fallback routes into the advisor.
func (a *FallbackAdvisor) LoadMappingText(content string) error {
	parsed, err := ParseFallbackTable(content)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mergeRoutesLocked(parsed)
	return nil
}

// LoadMappingFile loads and merges fallback routes from disk into the advisor.
func (a *FallbackAdvisor) LoadMappingFile(path string) error {
	parsed, err := LoadFallbackFile(path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mergeRoutesLocked(parsed)
	return nil
}

// SetCatalog updates the candidate model directory used for availability filtering
// and peer-tier recommendations.
func (a *FallbackAdvisor) SetCatalog(models []api.ModelInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setCatalogLocked(models)
}

// MarkUnavailable places a model on cooldown according to the failure diagnosis.
func (a *FallbackAdvisor) MarkUnavailable(model string, diag LimitDiagnosis, now time.Time) {
	key := strings.TrimSpace(model)
	if key == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	var window time.Duration
	switch diag.Kind {
	case QuotaExhausted:
		window = a.quotaCooldown
	case RateLimited, Overloaded:
		window = max(a.rateLimitCooldown, diag.RetryAfter)
	default:
		return
	}
	if diag.RetryAfter > window {
		window = diag.RetryAfter
	}
	a.cooldowns[strings.ToLower(key)] = cooldownEntry{
		kind:  diag.Kind,
		until: now.Add(window),
	}
}

// ClearCooldown removes any active cooldown entry for model.
func (a *FallbackAdvisor) ClearCooldown(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cooldowns, strings.ToLower(strings.TrimSpace(model)))
}

// Recommend returns the top fallback model and full ordered candidate list for
// primaryModel, skipping excluded and cooled-down models.
func (a *FallbackAdvisor) Recommend(primaryModel string, failureKind FailureKind, exclude []string, now time.Time) (string, []string) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	candidates := a.candidatesLocked(primaryModel, failureKind, exclude, now)
	if len(candidates) == 0 {
		return "", nil
	}
	return candidates[0], candidates
}

// Advise diagnoses a failed attempt and decides whether to retry with backoff,
// switch immediately to a peer-tier fallback model, compact context, or abort.
func (a *FallbackAdvisor) Advise(q RecoveryQuery) RecoveryDecision {
	now := q.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	diag := ClassifyErrorWithHeaders(q.ErrorText, q.Headers, now)

	a.mu.Lock()
	maxRetries := a.maxRetries
	if q.MaxRetries > 0 {
		maxRetries = q.MaxRetries
	}
	backoffCfg := a.backoff

	attempt := q.Attempt
	if attempt < 1 {
		attempt = 1
	}

	switch diag.Kind {
	case QuotaExhausted:
		// Never spin in retry loops on billing/credits/quota failures.
		if q.PrimaryModel != "" {
			a.markCooldownLocked(q.PrimaryModel, diag, now)
		}
		candidates := a.candidatesLocked(q.PrimaryModel, diag.Kind, q.TriedModels, now)
		a.mu.Unlock()
		if len(candidates) > 0 {
			return RecoveryDecision{
				Diagnosis:          diag,
				Action:             ActionSwitchFallback,
				Wait:               0,
				FallbackModel:      candidates[0],
				FallbackCandidates: candidates,
				Reason:             fmt.Sprintf("quota/credits exhausted on %s; switching immediately to fallback %s", q.PrimaryModel, candidates[0]),
			}
		}
		return RecoveryDecision{
			Diagnosis: diag,
			Action:    ActionAbort,
			Reason:    "quota/credits exhausted and no healthy fallback model is available; aborting without retry",
		}

	case RateLimited, Overloaded:
		exceededRetries := attempt > maxRetries
		excessiveWait := diag.RetryAfter > 2*backoffCfg.normalized().MaxDelay
		if !exceededRetries && !excessiveWait {
			a.mu.Unlock()
			wait := backoffCfg.Compute(attempt, diag.RetryAfter)
			return RecoveryDecision{
				Diagnosis: diag,
				Action:    ActionRetryBackoff,
				Wait:      wait,
				Reason:    fmt.Sprintf("%s on attempt %d/%d; retrying in %s", diag.Kind, attempt, maxRetries, wait.Round(time.Millisecond)),
			}
		}
		if q.PrimaryModel != "" {
			a.markCooldownLocked(q.PrimaryModel, diag, now)
		}
		candidates := a.candidatesLocked(q.PrimaryModel, diag.Kind, q.TriedModels, now)
		a.mu.Unlock()
		if len(candidates) > 0 {
			diag.ShouldFallback = true
			return RecoveryDecision{
				Diagnosis:          diag,
				Action:             ActionSwitchFallback,
				Wait:               0,
				FallbackModel:      candidates[0],
				FallbackCandidates: candidates,
				Reason:             fmt.Sprintf("%s persisted (%d attempts); switching to fallback %s", diag.Kind, attempt, candidates[0]),
			}
		}
		if !exceededRetries {
			wait := backoffCfg.Compute(attempt, diag.RetryAfter)
			return RecoveryDecision{
				Diagnosis: diag,
				Action:    ActionRetryBackoff,
				Wait:      wait,
				Reason:    fmt.Sprintf("%s with long server wait; no fallback configured, backing off %s", diag.Kind, wait.Round(time.Millisecond)),
			}
		}
		return RecoveryDecision{
			Diagnosis: diag,
			Action:    ActionAbort,
			Reason:    fmt.Sprintf("%s exhausted %d retry attempts and no fallback model is available", diag.Kind, maxRetries),
		}

	case ContextOverflow:
		a.mu.Unlock()
		return RecoveryDecision{
			Diagnosis: diag,
			Action:    ActionCompactContext,
			Reason:    "prompt exceeded model context window; compact conversation before retrying",
		}

	default:
		a.mu.Unlock()
		return RecoveryDecision{
			Diagnosis: diag,
			Action:    ActionAbort,
			Reason:    diag.Summary,
		}
	}
}

func (a *FallbackAdvisor) markCooldownLocked(model string, diag LimitDiagnosis, now time.Time) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return
	}
	window := a.rateLimitCooldown
	if diag.Kind == QuotaExhausted {
		window = a.quotaCooldown
	}
	if diag.RetryAfter > window {
		window = diag.RetryAfter
	}
	a.cooldowns[key] = cooldownEntry{
		kind:  diag.Kind,
		until: now.Add(window),
	}
}

func (a *FallbackAdvisor) providerHasQuotaCooldownLocked(providerName string, now time.Time) bool {
	prefix := strings.ToLower(strings.TrimSpace(providerName)) + "/"
	for k, cd := range a.cooldowns {
		if cd.kind == QuotaExhausted && now.Before(cd.until) && strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (a *FallbackAdvisor) mergeRoutesLocked(routes map[string][]string) {
	norm := normalizeRoutesMap(routes)
	for k, targets := range norm {
		a.routes[k] = appendUnique(a.routes[k], targets...)
	}
}

func (a *FallbackAdvisor) setCatalogLocked(models []api.ModelInfo) {
	a.catalog = append([]api.ModelInfo(nil), models...)
	a.catalogSet = make(map[string]api.ModelInfo, len(models)*2)
	for _, m := range models {
		key := strings.ToLower(strings.TrimSpace(m.Key()))
		if key != "" {
			a.catalogSet[key] = m
		}
		bare := strings.ToLower(strings.TrimSpace(m.ID))
		if bare != "" {
			if _, exists := a.catalogSet[bare]; !exists {
				a.catalogSet[bare] = m
			}
		}
	}
}

func (a *FallbackAdvisor) candidatesLocked(primaryModel string, kind FailureKind, exclude []string, now time.Time) []string {
	primary := strings.TrimSpace(primaryModel)
	primaryLower := strings.ToLower(primary)
	primaryProvider, primaryBare := splitProviderModel(primaryLower)
	primaryTier := InferModelTier(primary)

	blocked := make(map[string]bool, len(exclude)+2)
	if primaryLower != "" {
		blocked[primaryLower] = true
	}
	if primaryBare != "" {
		blocked[primaryBare] = true
	}
	for _, ex := range exclude {
		if k := strings.ToLower(strings.TrimSpace(ex)); k != "" {
			blocked[k] = true
		}
	}

	isUsable := func(candidate string) (string, bool) {
		cand := strings.TrimSpace(candidate)
		if cand == "" {
			return "", false
		}
		candLower := strings.ToLower(cand)
		_, candBare := splitProviderModel(candLower)
		if blocked[candLower] || (candBare != "" && candLower == primaryBare) {
			return "", false
		}
		if cd, ok := a.cooldowns[candLower]; ok && now.Before(cd.until) {
			return "", false
		}
		// When a live catalog is registered, resolve bare names and verify availability.
		if len(a.catalog) > 0 {
			info, ok := a.catalogSet[candLower]
			if !ok && candBare != "" {
				info, ok = a.catalogSet[candBare]
			}
			if !ok {
				return "", false
			}
			resolved := info.Key()
			resolvedLower := strings.ToLower(resolved)
			if blocked[resolvedLower] {
				return "", false
			}
			if cd, exists := a.cooldowns[resolvedLower]; exists && now.Before(cd.until) {
				return "", false
			}
			return resolved, true
		}
		return cand, true
	}

	var ordered []string
	seen := make(map[string]bool)
	addCandidate := func(cand string) {
		resolved, ok := isUsable(cand)
		if !ok {
			return
		}
		key := strings.ToLower(resolved)
		if seen[key] {
			return
		}
		seen[key] = true
		ordered = append(ordered, resolved)
	}

	// 1. Explicit configured fallback routes (exact key -> bare model -> provider wildcard).
	lookupKeys := []string{primaryLower}
	if primaryBare != "" && primaryBare != primaryLower {
		lookupKeys = append(lookupKeys, primaryBare)
	}
	if primaryProvider != "" {
		lookupKeys = append(lookupKeys, primaryProvider+"/*")
	}
	lookupKeys = append(lookupKeys, "*")
	for _, lk := range lookupKeys {
		for _, target := range a.routes[lk] {
			addCandidate(target)
		}
	}

	// 2. Peer-tier recommendations from the candidate catalog.
	if len(a.catalog) > 0 {
		var primaryInfo api.ModelInfo
		if info, ok := a.catalogSet[primaryLower]; ok {
			primaryInfo = info
		} else if primaryBare != "" {
			primaryInfo = a.catalogSet[primaryBare]
		}

		type scoredModel struct {
			key   string
			score int
		}
		var scored []scoredModel
		for _, m := range a.catalog {
			key := m.Key()
			resolved, ok := isUsable(key)
			if !ok || seen[strings.ToLower(resolved)] {
				continue
			}
			candProvider, _ := splitProviderModel(strings.ToLower(resolved))
			candTier := InferModelTier(resolved)

			score := 0
			// Peer capability tier gets highest priority.
			switch {
			case candTier == primaryTier:
				score += 100
			case primaryTier == TierFlagship && candTier == TierBalanced:
				score += 60
			case primaryTier == TierBalanced && candTier == TierFlagship:
				score += 55
			default:
				score += 20
			}

			// On account/provider quota exhaustion, strongly prefer a different
			// provider whose billing pool is independent.
			if kind == QuotaExhausted && primaryProvider != "" {
				if candProvider != "" && candProvider != primaryProvider {
					score += 50
				} else {
					score -= 30
				}
			} else if primaryProvider != "" && candProvider != primaryProvider {
				score += 10
			}

			// Penalize providers that already have another model in QuotaExhausted
			// cooldown (account-level credit depletion usually affects sibling models).
			if candProvider != "" && a.providerHasQuotaCooldownLocked(candProvider, now) {
				score -= 60
			}

			if primaryInfo.Reasoning && m.Reasoning {
				score += 15
			}
			if hasInputModality(primaryInfo.Input, "image") && hasInputModality(m.Input, "image") {
				score += 10
			}
			if primaryInfo.ContextWindow > 0 && m.ContextWindow >= primaryInfo.ContextWindow {
				score += 8
			}
			scored = append(scored, scoredModel{key: resolved, score: score})
		}

		sort.SliceStable(scored, func(i, j int) bool {
			if scored[i].score != scored[j].score {
				return scored[i].score > scored[j].score
			}
			return scored[i].key < scored[j].key
		})
		for _, item := range scored {
			addCandidate(item.key)
		}
	}

	return ordered
}

func inspectJSONError(text string) (statusCode int, providerCode string, message string, retryAfter time.Duration) {
	idx := strings.IndexByte(text, '{')
	if idx < 0 {
		return 0, "", "", 0
	}
	raw := strings.TrimSpace(text[idx:])
	if !json.Valid([]byte(raw)) {
		end := strings.LastIndexByte(raw, '}')
		if end <= 0 {
			return 0, "", "", 0
		}
		raw = raw[:end+1]
	}

	var payload struct {
		Status       int             `json:"status"`
		StatusCode   int             `json:"statusCode"`
		HTTPStatus   int             `json:"http_status"`
		Type         string          `json:"type"`
		Code         json.RawMessage `json:"code"`
		Message      string          `json:"message"`
		RetryAfter   *float64        `json:"retry_after"`
		RetryAfter2  *float64        `json:"retryAfter"`
		RetryAfterMs *float64        `json:"retry_after_ms"`
		RetryDelay   string          `json:"retryDelay"`
		Error        json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return 0, "", "", 0
	}

	statusCode = firstNonZeroInt(payload.Status, payload.StatusCode, payload.HTTPStatus)
	codeStr, codeInt := decodeCodeField(payload.Code)
	if statusCode == 0 && codeInt >= 400 && codeInt <= 599 {
		statusCode = codeInt
	}
	providerCode = firstString(codeStr, payload.Type)
	message = strings.TrimSpace(payload.Message)
	retryAfter = parseJSONRetryFields(payload.RetryAfter, payload.RetryAfter2, payload.RetryAfterMs, payload.RetryDelay)

	if len(bytes.TrimSpace(payload.Error)) > 0 {
		var nestedStr string
		if json.Unmarshal(payload.Error, &nestedStr) == nil && nestedStr != "" {
			if message == "" {
				message = nestedStr
			}
		} else {
			var nested struct {
				Status       int             `json:"status"`
				StatusCode   int             `json:"statusCode"`
				Type         string          `json:"type"`
				Code         json.RawMessage `json:"code"`
				Message      string          `json:"message"`
				RetryAfter   *float64        `json:"retry_after"`
				RetryAfter2  *float64        `json:"retryAfter"`
				RetryAfterMs *float64        `json:"retry_after_ms"`
				RetryDelay   string          `json:"retryDelay"`
				Details      []struct {
					RetryDelay string `json:"retryDelay"`
					Reason     string `json:"reason"`
				} `json:"details"`
			}
			if json.Unmarshal(payload.Error, &nested) == nil {
				statusCode = firstNonZeroInt(statusCode, nested.Status, nested.StatusCode)
				nCodeStr, nCodeInt := decodeCodeField(nested.Code)
				if statusCode == 0 && nCodeInt >= 400 && nCodeInt <= 599 {
					statusCode = nCodeInt
				}
				providerCode = firstString(nCodeStr, nested.Type, providerCode)
				message = firstString(nested.Message, message)
				if d := parseJSONRetryFields(nested.RetryAfter, nested.RetryAfter2, nested.RetryAfterMs, nested.RetryDelay); d > retryAfter {
					retryAfter = d
				}
				for _, det := range nested.Details {
					if providerCode == "" && det.Reason != "" {
						providerCode = det.Reason
					}
					if det.RetryDelay != "" {
						if d, err := time.ParseDuration(strings.ToLower(det.RetryDelay)); err == nil && d > retryAfter {
							retryAfter = d
						}
					}
				}
			}
		}
	}
	return statusCode, providerCode, message, retryAfter
}

func decodeCodeField(raw json.RawMessage) (string, int) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", 0
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		return strings.TrimSpace(s), n
	}
	var n int
	if json.Unmarshal(trimmed, &n) == nil {
		return strconv.Itoa(n), n
	}
	return "", 0
}

func parseJSONRetryFields(sec1, sec2, ms *float64, delayStr string) time.Duration {
	if ms != nil && *ms > 0 {
		return time.Duration(*ms * float64(time.Millisecond))
	}
	for _, s := range []*float64{sec1, sec2} {
		if s != nil && *s > 0 {
			return time.Duration(*s * float64(time.Second))
		}
	}
	if delayStr != "" {
		if d, ok := ParseRetryAfter(delayStr, time.Time{}); ok {
			return d
		}
	}
	return 0
}

func extractStatusCode(text string) int {
	for _, re := range []*regexp.Regexp{statusPrefixRe, statusLabelRe, statusPhraseRe} {
		if m := re.FindStringSubmatch(text); len(m) == 2 {
			if code, err := strconv.Atoi(m[1]); err == nil {
				return code
			}
		}
	}
	return 0
}

func summarizeErrorText(raw, fallback string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = secretTokenPattern.ReplaceAllString(s, "[REDACTED]")
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

func lookupHeader(headers map[string]string, key string) string {
	if len(headers) == 0 {
		return ""
	}
	if v, ok := headers[key]; ok {
		return strings.TrimSpace(v)
	}
	for k, v := range headers {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func normalizeRoutesMap(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, list := range in {
		key := strings.ToLower(cleanModelCell(k))
		if key == "" {
			continue
		}
		var cleaned []string
		for _, item := range list {
			for _, sub := range splitModelTargets(item) {
				if sub != "" {
					cleaned = appendUnique(cleaned, sub)
				}
			}
		}
		if len(cleaned) > 0 {
			out[key] = appendUnique(out[key], cleaned...)
		}
	}
	return out
}

func cleanModelCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`\"'")
	return strings.TrimSpace(s)
}

func isMarkdownTableHeaderOrSep(left, right string) bool {
	isSep := func(v string) bool {
		v = strings.Trim(v, ":- ")
		return v == ""
	}
	if isSep(left) || isSep(right) {
		return true
	}
	ll := strings.ToLower(left)
	switch ll {
	case "model", "primary", "primary model", "source", "from", "主模型", "原模型", "当前模型":
		return true
	}
	return false
}

func splitModelTargets(raw string) []string {
	replaced := strings.NewReplacer("->", ",", "=>", ",", "→", ",", ";", ",", "，", ",", "、", ",").Replace(raw)
	parts := strings.Split(replaced, ",")
	var out []string
	for _, p := range parts {
		cand := cleanModelCell(p)
		if cand == "" || cand == "-" || strings.EqualFold(cand, "none") || strings.EqualFold(cand, "n/a") {
			continue
		}
		// Strip trailing parenthetical notes like "openai/gpt-5.4 (same tier)".
		if idx := strings.Index(cand, "("); idx > 0 {
			cand = cleanModelCell(cand[:idx])
		}
		if idx := strings.Index(cand, "（"); idx > 0 {
			cand = cleanModelCell(cand[:idx])
		}
		if cand != "" && !strings.ContainsAny(cand, " \t") {
			out = appendUnique(out, cand)
		}
	}
	return out
}

func appendUnique(dst []string, items ...string) []string {
	for _, item := range items {
		dup := false
		for _, existing := range dst {
			if strings.EqualFold(existing, item) {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, item)
		}
	}
	return dst
}

func splitProviderModel(key string) (providerName, modelID string) {
	if p, m, ok := strings.Cut(key, "/"); ok {
		return p, m
	}
	return "", key
}

func hasInputModality(inputs []string, target string) bool {
	for _, in := range inputs {
		if strings.EqualFold(strings.TrimSpace(in), target) {
			return true
		}
	}
	return false
}

func firstNonZeroInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
