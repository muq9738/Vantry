package telemetry

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MinTargetTitleRunes = 18
	MaxTitleRunes       = 28
	DefaultThreadTitle  = "新会话"
)

// Summarizer generates a short, precise session title asynchronously using an
// optional lightweight model once the first turn finishes.
type Summarizer func(ctx context.Context, prompt string) (string, error)

var (
	fencedCodeBlockRe = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	unclosedFenceRe   = regexp.MustCompile("(?s)(?:```|~~~).*$")
	htmlTagRe         = regexp.MustCompile(`<[^>]+>`)
	mdImageRe         = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)
	mdLinkRe          = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	ghIssueURLRe      = regexp.MustCompile(`https?://github\.com/[^/\s]+/[^/\s]+/(?:issues|pull)/(\d+)[^\s)]*`)
	rawURLRe          = regexp.MustCompile(`https?://[^\s)>\]]+`)
	unixLongPathRe    = regexp.MustCompile(`(?:~|/[A-Za-z0-9._\-]+)(?:/[A-Za-z0-9._\-]+){2,}|[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+){2,}`)
	winLongPathRe     = regexp.MustCompile(`[A-Za-z]:\\(?:[^\\\s:*?"<>|]+\\){2,}[^\\\s:*?"<>|]+`)
	listMarkerRe      = regexp.MustCompile(`^(?:[-*+•]\s+(?:\[[ xX]\]\s+)?|\d+[.)、]\s+)`)
	headingMarkerRe   = regexp.MustCompile(`^#{1,6}\s+`)
	quoteMarkerRe     = regexp.MustCompile(`^(?:>\s*)+`)
	stackTraceLineRe  = regexp.MustCompile(`(?i)^\s*(?:at\s+\S+|\S+\.go:\d+|file\s+".+",\s+line\s+\d+|goroutine\s+\d+\s+\[)`)
	roleBoilerplateRe = regexp.MustCompile(`(?i)^act\s+as\s+(?:the\s+|an?\s+)?[^.!?\n]+(?:for\s+this\s+task)?[.!?:]*$`)

	enPolitePrefixes = []string{
		"hi there", "hello there", "hey there",
		"good morning", "good afternoon", "good evening",
		"hi", "hello", "hey",
		"could you please help me to", "could you please help me", "could you please", "could you",
		"can you please help me to", "can you please help me", "can you please", "can you",
		"would you mind helping me", "would you please", "would you",
		"please help me to", "please help me", "help me to", "help me",
		"i would like you to", "i'd like you to", "i want you to", "i need you to",
		"i would like to", "i want to", "i need to",
		"please", "let's",
	}

	zhPolitePrefixes = []string{
		"各位好", "大家好", "早上好", "下午好", "晚上好",
		"您好", "你好", "哈喽", "嗨",
		"麻烦请帮我", "麻烦帮我", "麻烦请你", "麻烦你", "麻烦", "劳烦帮我", "劳烦",
		"请问能不能帮我", "请问能否帮我", "请问可以帮我", "请问一下", "请问下", "请问",
		"能不能请帮我", "能不能帮我", "能不能", "能否帮我", "能否",
		"可以帮我", "可不可以帮我",
		"请帮我看一下", "请帮我看下", "请帮我看看", "请帮我",
		"帮我看一下", "帮我看下", "帮我看看", "帮忙看一下", "帮忙看下", "帮忙看看",
		"帮我", "帮忙",
		"我想请你帮我", "我想让你帮我", "我想请你", "我想让你", "我需要你帮我", "我需要你",
		"我想", "我需要", "希望你能", "希望你",
		"现在请帮我", "现在请", "请你", "请协助", "请",
		"看一下", "看下",
	}

	assistantLeadPrefixes = []string{
		"好的", "没问题", "当然可以", "当然", "收到", "明白了", "明白",
		"我来帮你", "我来为您", "我来", "下面我来", "接下来我来", "我们可以", "让我们",
		"这个报错是因为", "这个问题是因为", "原因在于", "这是因为",
		"sure", "certainly", "of course", "no problem", "got it",
		"i'll help you", "i will help you", "i can help you", "let me", "here is", "here's",
	}

	trailingDanglingZh = []string{"的", "和", "与", "及", "以及", "并", "在", "把", "将", "对", "向", "从", "里", "中"}
	trailingDanglingEn = map[string]bool{
		"the": true, "a": true, "an": true, "to": true, "for": true,
		"in": true, "on": true, "at": true, "by": true, "with": true,
		"and": true, "or": true, "of": true, "from": true, "into": true,
	}
)

// DeriveTitle locally extracts a crisp 18–28 character intent title from the
// first user message (falling back to code-block clues or the first assistant
// reply when the user prompt has no prose), stripping code fences, long path
// prefixes, Markdown syntax, and conversational pleasantries without breaking words.
func DeriveTitle(firstUserMessage, firstAssistantReply string) string {
	candidate := extractCoreIntent(firstUserMessage, false)
	if utf8.RuneCountInString(candidate) < 2 && strings.TrimSpace(firstAssistantReply) != "" {
		if fromReply := extractCoreIntent(firstAssistantReply, true); utf8.RuneCountInString(fromReply) >= 2 {
			candidate = fromReply
		}
	}
	if utf8.RuneCountInString(candidate) < 2 {
		if fromCode := extractCodeFallback(firstUserMessage); fromCode != "" {
			candidate = fromCode
		}
	}
	if strings.TrimSpace(candidate) == "" {
		return DefaultThreadTitle
	}
	return fitTitleWidth(candidate, MinTargetTitleRunes, MaxTitleRunes)
}

// SanitizeModelTitle cleans a raw title returned by an LLM Summarizer, removing
// label prefixes, wrapping quotes, Markdown, and clamping width safely.
func SanitizeModelTitle(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = fencedCodeBlockRe.ReplaceAllString(s, " ")
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	for _, label := range []string{"标题：", "标题:", "会话标题：", "会话标题:", "Title:", "title:", "Session Title:"} {
		if strings.HasPrefix(s, label) {
			s = strings.TrimSpace(strings.TrimPrefix(s, label))
		}
	}
	s = stripInlineMarkdown(s)
	s = trimSurroundingQuotesAndPunct(s)
	if s == "" {
		return ""
	}
	return fitTitleWidth(s, MinTargetTitleRunes, MaxTitleRunes)
}

// BuildTitlePrompt constructs a compact, credential-redacted summarization prompt
// suitable for passing to a lightweight model Summarizer.
func BuildTitlePrompt(firstUserMessage, firstAssistantReply string) string {
	userExcerpt := compactPromptExcerpt(firstUserMessage, 400)
	replyExcerpt := compactPromptExcerpt(firstAssistantReply, 300)
	var b strings.Builder
	b.WriteString("Generate a concise session title (18-28 characters, matching the user's primary language, no quotes, no trailing period, no pleasantries) summarizing the core task:\n\n")
	b.WriteString("User: ")
	b.WriteString(userExcerpt)
	if replyExcerpt != "" {
		b.WriteString("\nAssistant: ")
		b.WriteString(replyExcerpt)
	}
	b.WriteString("\n\nTitle:")
	return b.String()
}

// TitleGenerator pairs zero-latency local heuristic derivation with optional
// asynchronous model-driven title refinement (mirroring the thread title refinement lifecycle).
type TitleGenerator struct {
	summarizer Summarizer
	timeout    time.Duration
	wg         sync.WaitGroup
}

// TitleOption configures a TitleGenerator.
type TitleOption func(*TitleGenerator)

// WithSummarizerTimeout sets the per-call deadline for asynchronous model summarization.
func WithSummarizerTimeout(d time.Duration) TitleOption {
	return func(g *TitleGenerator) {
		if d > 0 {
			g.timeout = d
		}
	}
}

// NewTitleGenerator creates a TitleGenerator with an optional Summarizer.
func NewTitleGenerator(summarizer Summarizer, opts ...TitleOption) *TitleGenerator {
	g := &TitleGenerator{
		summarizer: summarizer,
		timeout:    15 * time.Second,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

// Derive synchronously returns the zero-latency heuristic title.
func (g *TitleGenerator) Derive(firstUserMessage, firstAssistantReply string) string {
	return DeriveTitle(firstUserMessage, firstAssistantReply)
}

// Regenerate runs the optional Summarizer synchronously and falls back to
// DeriveTitle if no Summarizer is configured or if model generation fails.
func (g *TitleGenerator) Regenerate(ctx context.Context, firstUserMessage, firstAssistantReply string) (string, bool, error) {
	heuristic := DeriveTitle(firstUserMessage, firstAssistantReply)
	if g == nil || g.summarizer == nil {
		return heuristic, false, nil
	}
	runCtx := ctx
	if runCtx == nil {
		runCtx = context.Background()
	}
	var cancel context.CancelFunc
	if g.timeout > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, g.timeout)
		defer cancel()
	}
	prompt := BuildTitlePrompt(firstUserMessage, firstAssistantReply)
	raw, err := g.summarizer(runCtx, prompt)
	if err != nil {
		return heuristic, false, fmt.Errorf("summarize session title: %w", err)
	}
	cleaned := SanitizeModelTitle(raw)
	if utf8.RuneCountInString(cleaned) < 2 {
		return heuristic, false, nil
	}
	return cleaned, true, nil
}

// GenerateAsync immediately returns the heuristic title for zero-latency UI
// display and, when a Summarizer is configured, refines the title in a background
// goroutine and invokes onTitle with the sanitized model title.
func (g *TitleGenerator) GenerateAsync(ctx context.Context, firstUserMessage, firstAssistantReply string, onTitle func(title string)) string {
	heuristic := DeriveTitle(firstUserMessage, firstAssistantReply)
	if g == nil || g.summarizer == nil || onTitle == nil {
		return heuristic
	}
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		refined, usedModel, err := g.Regenerate(parentCtx, firstUserMessage, firstAssistantReply)
		if err != nil || !usedModel || refined == "" {
			return
		}
		onTitle(refined)
	}()
	return heuristic
}

// Wait blocks until all in-flight asynchronous title generation goroutines finish.
func (g *TitleGenerator) Wait() {
	if g == nil {
		return
	}
	g.wg.Wait()
}

func extractCoreIntent(raw string, isAssistant bool) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = stripInjectedXMLBlocks(text)
	text = fencedCodeBlockRe.ReplaceAllString(text, "\n")
	text = unclosedFenceRe.ReplaceAllString(text, "\n")

	lines := strings.Split(text, "\n")
	for _, rawLine := range lines {
		line := cleanSingleLine(rawLine)
		if line == "" || isNoiseLine(line) {
			continue
		}
		// Split multi-sentence lines so an opening greeting ("你好！") does not
		// swallow the actual request in the second sentence.
		clauses := splitSentences(line)
		for _, clause := range clauses {
			cleaned := stripPolitePrefixes(clause, isAssistant)
			cleaned = trimSurroundingQuotesAndPunct(cleaned)
			if isMeaningfulIntent(cleaned) {
				return polishLeadingCase(cleaned)
			}
		}
	}
	return ""
}

func cleanSingleLine(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return ""
	}
	s = quoteMarkerRe.ReplaceAllString(s, "")
	s = headingMarkerRe.ReplaceAllString(s, "")
	s = listMarkerRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Preserve Markdown link labels and condense GitHub issue/PR URLs to #N.
	s = mdImageRe.ReplaceAllString(s, "$1")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = ghIssueURLRe.ReplaceAllString(s, "#$1")
	s = rawURLRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = stripInlineMarkdown(s)
	s = shortenLongPaths(s)
	s = secretTokenPattern.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

func stripInlineMarkdown(s string) string {
	r := strings.NewReplacer(
		"**", "",
		"__", "",
		"~~", "",
		"`", "",
	)
	s = r.Replace(s)
	// Strip standalone emphasis asterisks around words while keeping pointer/glob uses modest.
	s = strings.Trim(s, "*_ ")
	return s
}

func shortenLongPaths(s string) string {
	s = winLongPathRe.ReplaceAllStringFunc(s, func(match string) string {
		idx := strings.LastIndexByte(match, '\\')
		if idx >= 0 && idx+1 < len(match) {
			return match[idx+1:]
		}
		return match
	})
	s = unixLongPathRe.ReplaceAllStringFunc(s, func(match string) string {
		trimmed := strings.TrimRight(match, "/")
		idx := strings.LastIndexByte(trimmed, '/')
		if idx >= 0 && idx+1 < len(trimmed) {
			return trimmed[idx+1:]
		}
		return match
	})
	return s
}

func isNoiseLine(line string) bool {
	trimmed := strings.Trim(line, "-=_*# \t")
	if trimmed == "" {
		return true
	}
	if stackTraceLineRe.MatchString(line) {
		return true
	}
	if roleBoilerplateRe.MatchString(line) {
		return true
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "谢谢", "谢谢！", "感谢", "thanks", "thank you", "thx":
		return true
	}
	return false
}

func splitSentences(line string) []string {
	var parts []string
	var buf strings.Builder
	runes := []rune(line)
	for i, r := range runes {
		if r == '。' || r == '！' || r == '？' || r == '；' || r == '!' || r == '?' || r == ';' {
			if piece := strings.TrimSpace(buf.String()); piece != "" {
				parts = append(parts, piece)
			}
			buf.Reset()
			continue
		}
		// Treat ASCII period as sentence break only when followed by whitespace
		// (never split `worker.go` or `v1.2.3`).
		if r == '.' && i+1 < len(runes) && unicode.IsSpace(runes[i+1]) {
			if piece := strings.TrimSpace(buf.String()); piece != "" {
				parts = append(parts, piece)
			}
			buf.Reset()
			continue
		}
		buf.WriteRune(r)
	}
	if tail := strings.TrimSpace(buf.String()); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

func stripPolitePrefixes(s string, isAssistant bool) string {
	cur := strings.TrimSpace(s)
	for pass := 0; pass < 8; pass++ {
		prev := cur
		cur = trimLeadingConnectors(cur)

		if isAssistant {
			for _, p := range assistantLeadPrefixes {
				if next, ok := cutLeadingPhrase(cur, p); ok {
					cur = next
				}
			}
		}
		for _, p := range zhPolitePrefixes {
			if next, ok := cutLeadingPhrase(cur, p); ok {
				cur = next
			}
		}
		for _, p := range enPolitePrefixes {
			if next, ok := cutLeadingEnglishPhrase(cur, p); ok {
				cur = next
			}
		}
		cur = trimLeadingConnectors(cur)
		if cur == prev {
			break
		}
	}
	return cur
}

func cutLeadingPhrase(s, prefix string) (string, bool) {
	if isASCIIAlphaString(prefix) {
		return cutLeadingEnglishPhrase(s, prefix)
	}
	if strings.HasPrefix(s, prefix) {
		rest := strings.TrimSpace(s[len(prefix):])
		// Avoid stripping "查看" when prefix was "看" (we only list multi-char safe prefixes),
		// and allow empty rest so pure greetings ("你好") vanish completely.
		return rest, true
	}
	return s, false
}

func cutLeadingEnglishPhrase(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return s, false
	}
	if !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	if len(s) == len(prefix) {
		return "", true
	}
	nextRune, _ := utf8.DecodeRuneInString(s[len(prefix):])
	if isASCIIWordRune(nextRune) {
		return s, false
	}
	return strings.TrimSpace(s[len(prefix):]), true
}

func trimLeadingConnectors(s string) string {
	s = strings.TrimLeftFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，.。!！?？:：;；、~～-—", r)
	})
	for _, filler := range []string{"一下 ", "一下，", "一下,"} {
		if strings.HasPrefix(s, filler) {
			s = strings.TrimSpace(strings.TrimPrefix(s, filler))
		}
	}
	return s
}

func trimSurroundingQuotesAndPunct(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`“”‘’《》「」『』[]()")
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，.。!！?？:：;；、-—", r)
	})
	return strings.TrimSpace(s)
}

func isMeaningfulIntent(s string) bool {
	if utf8.RuneCountInString(s) < 2 {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

func polishLeadingCase(s string) string {
	if s == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(s)
	if first >= 'a' && first <= 'z' {
		// Preserve lowercase filenames/identifiers like `worker.go` or `ffgrep`.
		rest := s[size:]
		spaceIdx := strings.IndexByte(rest, ' ')
		firstToken := s
		if spaceIdx >= 0 {
			firstToken = s[:size+spaceIdx]
		}
		if strings.ContainsAny(firstToken, "./_-0123456789") {
			return s
		}
		return string(unicode.ToUpper(first)) + rest
	}
	return s
}

func extractCodeFallback(raw string) string {
	matches := fencedCodeBlockRe.FindAllString(raw, -1)
	if len(matches) == 0 {
		if m := unclosedFenceRe.FindString(raw); m != "" {
			matches = []string{m}
		}
	}
	for _, block := range matches {
		lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if i == 0 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
				continue
			}
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				continue
			}
			if trimmed == "" || stackTraceLineRe.MatchString(trimmed) {
				continue
			}
			cleaned := shortenLongPaths(trimmed)
			cleaned = strings.Trim(cleaned, "{}[]();, ")
			cleaned = strings.Join(strings.Fields(cleaned), " ")
			if isMeaningfulIntent(cleaned) {
				return cleaned
			}
		}
	}
	return ""
}

// fitTitleWidth enforces a UTF-8 safe, non-word-breaking length budget targeting
// [minRunes, maxRunes] runes when truncating longer inputs.
func fitTitleWidth(s string, minRunes, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return trimSurroundingQuotesAndPunct(s)
	}

	// 1. Prefer a natural clause delimiter inside [minRunes, maxRunes].
	for i := maxRunes; i >= minRunes; i-- {
		if isClauseBreakRune(runes[i]) {
			cand := trimDanglingTail(runes[:i], minRunes)
			if len([]rune(cand)) >= minRunes-2 {
				return cand
			}
		}
	}

	// 2. If cutting at maxRunes would split an ASCII word/number, walk back to
	// a clean word or sub-identifier boundary.
	cut := maxRunes
	if isASCIIWordRune(runes[cut-1]) && isASCIIWordRune(runes[cut]) {
		wordStart := cut - 1
		for wordStart > 0 && isASCIIWordRune(runes[wordStart-1]) {
			wordStart--
		}
		if wordStart >= minRunes {
			cut = wordStart
		} else {
			// Try sub-token boundary (`.` / `-` / `_` / `/`) inside [minRunes, maxRunes]
			// so long filenames like `Refactor websocket_handler.ts` become `Refactor websocket_handler`.
			subCut := -1
			for i := maxRunes; i >= minRunes; i-- {
				if runes[i] == '.' || runes[i] == '/' || runes[i] == '-' || runes[i] == '_' {
					subCut = i
					break
				}
			}
			if subCut >= minRunes {
				cut = subCut
			} else if wordStart >= 12 {
				cut = wordStart
			}
		}
	} else {
		// Not inside an ASCII word: prefer whitespace boundary near maxRunes if close.
		for i := maxRunes; i >= minRunes; i-- {
			if unicode.IsSpace(runes[i]) {
				cut = i
				break
			}
		}
	}

	return trimDanglingTail(runes[:cut], minRunes)
}

func trimDanglingTail(runes []rune, minRunes int) string {
	s := trimSurroundingQuotesAndPunct(string(runes))
	if s == "" {
		return ""
	}

	// Strip dangling trailing English prepositions/conjunctions when safe.
	if idx := strings.LastIndexByte(s, ' '); idx > 0 {
		lastWord := strings.ToLower(strings.TrimSpace(s[idx+1:]))
		if trailingDanglingEn[lastWord] {
			prefix := trimSurroundingQuotesAndPunct(s[:idx])
			if utf8.RuneCountInString(prefix) >= minRunes-2 {
				s = prefix
			}
		}
	}

	// Strip dangling trailing Chinese particles when it keeps sufficient length.
	for _, particle := range trailingDanglingZh {
		if strings.HasSuffix(s, particle) {
			prefix := trimSurroundingQuotesAndPunct(strings.TrimSuffix(s, particle))
			if utf8.RuneCountInString(prefix) >= minRunes {
				s = prefix
				break
			}
		}
	}

	return s
}

func isClauseBreakRune(r rune) bool {
	switch r {
	case ',', '，', '、', ';', '；', ':', '：', '|', '—':
		return true
	default:
		return false
	}
}

func isASCIIWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isASCIIAlphaString(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func stripInjectedXMLBlocks(s string) string {
	for _, tag := range []string{
		"system-reminder",
		"system_instruction",
		"project_context",
		"context",
		"attachments",
		"attachment",
		"env",
	} {
		openTag := "<" + tag
		closeTag := "</" + tag + ">"
		searchFrom := 0
		for searchFrom < len(s) {
			lower := strings.ToLower(s)
			rel := strings.Index(lower[searchFrom:], openTag)
			if rel < 0 {
				break
			}
			start := searchFrom + rel
			afterOpen := start + len(openTag)
			if afterOpen < len(lower) {
				ch := lower[afterOpen]
				if ch != '>' && ch != ' ' && ch != '\t' && ch != '\n' {
					searchFrom = afterOpen
					continue
				}
			}
			end := strings.Index(lower[afterOpen:], closeTag)
			if end < 0 {
				break
			}
			s = s[:start] + "\n" + s[afterOpen+end+len(closeTag):]
			searchFrom = start + 1
		}
	}
	return s
}

func compactPromptExcerpt(raw string, maxRunes int) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = fencedCodeBlockRe.ReplaceAllString(s, " [code] ")
	s = unclosedFenceRe.ReplaceAllString(s, " [code] ")
	s = secretTokenPattern.ReplaceAllString(s, "[REDACTED]")
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > maxRunes {
		s = string(runes[:maxRunes]) + "..."
	}
	return strings.TrimSpace(s)
}
