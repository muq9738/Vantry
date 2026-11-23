// Package pr inspects GitHub Pull Request status via the local gh CLI and
// evaluates state transitions for automatic thread wakeups.
package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostprocess"
)

const (
	defaultGHTimeout = 30 * time.Second
	maxGHOutputBytes = 2 << 20
	maxCommentBody   = 512
)

var prJSONFields = strings.Join([]string{
	"number",
	"title",
	"url",
	"state",
	"isDraft",
	"headRefName",
	"baseRefName",
	"headRefOid",
	"reviewDecision",
	"mergeStateStatus",
	"statusCheckRollup",
	"comments",
	"reviews",
	"updatedAt",
}, ",")

// ChecksSummary aggregates status checks and workflow runs attached to a PR.
type ChecksSummary struct {
	Total       int      `json:"total"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	Pending     int      `json:"pending"`
	FailedNames []string `json:"failedNames,omitempty"`
}

// PRInfo represents normalized business fields of a GitHub Pull Request.
type PRInfo struct {
	Number              int           `json:"number"`
	Title               string        `json:"title"`
	URL                 string        `json:"url"`
	State               string        `json:"state"`
	IsDraft             bool          `json:"isDraft"`
	HeadRefName         string        `json:"headRefName,omitempty"`
	BaseRefName         string        `json:"baseRefName,omitempty"`
	HeadRefOID          string        `json:"headRefOid,omitempty"`
	ReviewDecision      string        `json:"reviewDecision,omitempty"`
	MergeStateStatus    string        `json:"mergeStateStatus,omitempty"`
	ChecksSummary       ChecksSummary `json:"checksSummary"`
	LatestCommentID     string        `json:"latestCommentId,omitempty"`
	LatestCommentAuthor string        `json:"latestCommentAuthor,omitempty"`
	LatestCommentBody   string        `json:"latestCommentBody,omitempty"`
	UpdatedAt           time.Time     `json:"updatedAt"`
}

// Runner executes a gh CLI command in dir and returns stdout, stderr, and error.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) (stdout string, stderr string, err error)
}

// RunnerFunc adapts a plain function to Runner.
type RunnerFunc func(ctx context.Context, dir string, args ...string) (stdout string, stderr string, err error)

func (f RunnerFunc) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	return f(ctx, dir, args...)
}

type boundedBuffer struct {
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.data.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || n > 0
		return n, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.data.Write(p)
	return n, nil
}

func (b *boundedBuffer) String() string {
	return strings.ToValidUTF8(b.data.String(), "")
}

type execGHRunner struct{}

func (execGHRunner) Run(ctx context.Context, dir string, args ...string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return "", "", fmt.Errorf("检查工作区目录失败：%w", err)
		}
		if !info.IsDir() {
			return "", "", fmt.Errorf("工作区路径不是目录：%s", dir)
		}
	}
	cmd := hostprocess.NewContext(ctx, "gh", args...)
	cmd.Dir = dir
	cmd.Env = append(workspace.GitEnvironment(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	defer cmd.Close()
	cmd.WaitDelay = time.Second

	stdout := boundedBuffer{limit: maxGHOutputBytes}
	stderr := boundedBuffer{limit: maxGHOutputBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if cmd.Process != nil && (ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay)) {
			_ = cmd.KillTree()
		}
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return stdout.String(), stderr.String(), err
	}
	if stdout.truncated {
		return "", stderr.String(), errors.New("gh 输出超过安全大小上限（2 MiB）")
	}
	return stdout.String(), stderr.String(), nil
}

// Client queries GitHub Pull Request metadata through a configurable Runner.
type Client struct {
	Runner  Runner
	Timeout time.Duration
}

// NewClient constructs a PR Client. When runner is nil, the local gh CLI is used.
func NewClient(runner Runner) *Client {
	if runner == nil {
		runner = execGHRunner{}
	}
	return &Client{
		Runner:  runner,
		Timeout: defaultGHTimeout,
	}
}

var defaultClient = NewClient(nil)

// InspectBranchPR inspects the PR associated with the current branch in workspacePath.
// When the branch has no associated PR or the workspace has no GitHub remote,
// it returns (nil, nil) instead of an error.
func InspectBranchPR(ctx context.Context, workspacePath string) (*PRInfo, error) {
	return defaultClient.InspectBranchPR(ctx, workspacePath)
}

// InspectPRByURL validates prURL and reads the corresponding PR via gh.
func InspectPRByURL(ctx context.Context, workspacePath, prURL string) (*PRInfo, error) {
	return defaultClient.InspectPRByURL(ctx, workspacePath, prURL)
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	timeout := defaultGHTimeout
	if c != nil && c.Timeout > 0 {
		timeout = c.Timeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (c *Client) runner() Runner {
	if c != nil && c.Runner != nil {
		return c.Runner
	}
	return execGHRunner{}
}

// InspectBranchPR queries the PR bound to the checked-out branch in workspacePath.
func (c *Client) InspectBranchPR(ctx context.Context, workspacePath string) (*PRInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return nil, errors.New("工作区路径不能为空")
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	stdout, stderr, err := c.runner().Run(ctx, workspacePath, "pr", "view", "--json", prJSONFields)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		if isAbsentPRorGitHubRemote(stderr, err) {
			return nil, nil
		}
		detail := strings.TrimSpace(stderr)
		if detail != "" {
			return nil, fmt.Errorf("查询当前分支 PR 状态失败：%w (%s)", err, detail)
		}
		return nil, fmt.Errorf("查询当前分支 PR 状态失败：%w", err)
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	return parsePRViewJSON(stdout)
}

// InspectPRByURL validates a GitHub PR URL and queries its live status.
func (c *Client) InspectPRByURL(ctx context.Context, workspacePath, prURL string) (*PRInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonicalURL, err := ValidatePRURL(prURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	stdout, stderr, err := c.runner().Run(ctx, strings.TrimSpace(workspacePath), "pr", "view", canonicalURL, "--json", prJSONFields)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail != "" {
			return nil, fmt.Errorf("查询指定 PR (%s) 失败：%w (%s)", canonicalURL, err, detail)
		}
		return nil, fmt.Errorf("查询指定 PR (%s) 失败：%w", canonicalURL, err)
	}
	return parsePRViewJSON(stdout)
}

// ValidatePRURL validates and canonicalizes an https://<host>/<owner>/<repo>/pull/<number> URL.
func ValidatePRURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("PR URL 不能为空")
	}
	if strings.IndexFunc(rawURL, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("PR URL 包含非法空白或控制字符")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("PR URL 格式无效：%w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("PR URL 必须使用 https 协议")
	}
	if parsed.User != nil {
		return "", errors.New("PR URL 不允许包含认证凭据")
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host == "" || !strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return "", errors.New("PR URL 主机名无效")
	}
	port := parsed.Port()
	if port != "" {
		if n, portErr := strconv.Atoi(port); portErr != nil || n < 1 || n > 65535 {
			return "", errors.New("PR URL 端口无效")
		}
		host = host + ":" + port
	}

	trimmedPath := strings.Trim(parsed.Path, "/")
	segments := strings.Split(trimmedPath, "/")
	if len(segments) < 4 || segments[2] != "pull" {
		return "", errors.New("PR URL 路径必须符合 /<owner>/<repo>/pull/<number> 格式")
	}
	// Allow standard PR tab suffixes (/files, /commits, /checks) while rejecting arbitrary paths.
	if len(segments) > 5 || (len(segments) == 5 && segments[4] != "files" && segments[4] != "commits" && segments[4] != "checks") {
		return "", errors.New("PR URL 路径包含不支持的子路径")
	}
	owner, repo, numberText := segments[0], segments[1], segments[3]
	if !validSlugSegment(owner) || !validSlugSegment(repo) {
		return "", errors.New("PR URL 中的仓库 owner 或 repo 名称无效")
	}
	num, err := strconv.Atoi(numberText)
	if err != nil || num <= 0 {
		return "", errors.New("PR URL 中的 PR 编号必须是正整数")
	}
	return fmt.Sprintf("https://%s/%s/%s/pull/%d", host, owner, repo, num), nil
}

func validSlugSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	for _, r := range seg {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func isAbsentPRorGitHubRemote(stderr string, err error) bool {
	combined := strings.ToLower(stderr + "\n" + err.Error())
	absentIndicators := []string{
		"no pull requests found",
		"no open pull requests found",
		"no git remotes found",
		"none of the git remotes configured",
		"could not determine base repository",
		"could not resolve to a repository",
		"could not determine current branch",
		"not a git repository",
	}
	for _, indicator := range absentIndicators {
		if strings.Contains(combined, indicator) {
			return true
		}
	}
	return false
}

type ghAuthor struct {
	Login string `json:"login"`
}

type ghStatusCheckItem struct {
	TypeName     string `json:"__typename"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	Context      string `json:"context"`
	State        string `json:"state"`
}

type ghCommentItem struct {
	ID        string    `json:"id"`
	Author    ghAuthor  `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

type ghReviewItem struct {
	ID          string    `json:"id"`
	Author      ghAuthor  `json:"author"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
}

type ghPRPayload struct {
	Number            int                 `json:"number"`
	Title             string              `json:"title"`
	URL               string              `json:"url"`
	State             string              `json:"state"`
	IsDraft           bool                `json:"isDraft"`
	HeadRefName       string              `json:"headRefName"`
	BaseRefName       string              `json:"baseRefName"`
	HeadRefOID        string              `json:"headRefOid"`
	ReviewDecision    string              `json:"reviewDecision"`
	MergeStateStatus  string              `json:"mergeStateStatus"`
	StatusCheckRollup []ghStatusCheckItem `json:"statusCheckRollup"`
	Comments          []ghCommentItem     `json:"comments"`
	Reviews           []ghReviewItem      `json:"reviews"`
	UpdatedAt         time.Time           `json:"updatedAt"`
}

func parsePRViewJSON(raw string) (*PRInfo, error) {
	var payload ghPRPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("解析 gh PR JSON 失败：%w", err)
	}
	if payload.Number <= 0 {
		return nil, errors.New("gh 返回的 PR 编号无效")
	}

	checks := summarizeChecks(payload.StatusCheckRollup)
	commentID, commentAuthor, commentBody := extractLatestComment(payload.Comments, payload.Reviews)

	return &PRInfo{
		Number:              payload.Number,
		Title:               payload.Title,
		URL:                 payload.URL,
		State:               strings.ToUpper(strings.TrimSpace(payload.State)),
		IsDraft:             payload.IsDraft,
		HeadRefName:         strings.TrimSpace(payload.HeadRefName),
		BaseRefName:         strings.TrimSpace(payload.BaseRefName),
		HeadRefOID:          strings.TrimSpace(payload.HeadRefOID),
		ReviewDecision:      strings.ToUpper(strings.TrimSpace(payload.ReviewDecision)),
		MergeStateStatus:    strings.ToUpper(strings.TrimSpace(payload.MergeStateStatus)),
		ChecksSummary:       checks,
		LatestCommentID:     commentID,
		LatestCommentAuthor: commentAuthor,
		LatestCommentBody:   truncateUTF8(strings.TrimSpace(commentBody), maxCommentBody),
		UpdatedAt:           payload.UpdatedAt,
	}, nil
}

func summarizeChecks(items []ghStatusCheckItem) ChecksSummary {
	var summary ChecksSummary
	var failedNames []string

	for _, item := range items {
		label := strings.TrimSpace(item.Name)
		if label == "" {
			label = strings.TrimSpace(item.Context)
		}
		if label == "" {
			label = "(unnamed-check)"
		}

		// StatusContext uses State directly when Status/Conclusion are absent.
		if strings.EqualFold(item.TypeName, "StatusContext") || (item.Status == "" && item.Conclusion == "" && item.State != "") {
			switch strings.ToUpper(strings.TrimSpace(item.State)) {
			case "SUCCESS":
				summary.Passed++
			case "FAILURE", "ERROR":
				summary.Failed++
				failedNames = append(failedNames, label)
			default:
				summary.Pending++
			}
			continue
		}

		status := strings.ToUpper(strings.TrimSpace(item.Status))
		conclusion := strings.ToUpper(strings.TrimSpace(item.Conclusion))
		if status != "" && status != "COMPLETED" {
			summary.Pending++
			continue
		}
		switch conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			summary.Passed++
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR", "STALE":
			summary.Failed++
			failedNames = append(failedNames, label)
		default:
			summary.Pending++
		}
	}

	summary.Total = summary.Passed + summary.Failed + summary.Pending
	if len(failedNames) > 0 {
		slices.Sort(failedNames)
		summary.FailedNames = slices.Compact(failedNames)
	}
	return summary
}

func extractLatestComment(comments []ghCommentItem, reviews []ghReviewItem) (id, author, body string) {
	type candidate struct {
		id     string
		author string
		body   string
		at     time.Time
		order  int
	}
	var best *candidate
	seq := 0
	consider := func(cid, cauthor, cbody string, at time.Time) {
		cid = strings.TrimSpace(cid)
		if cid == "" {
			return
		}
		seq++
		cur := candidate{
			id:     cid,
			author: strings.TrimSpace(cauthor),
			body:   cbody,
			at:     at,
			order:  seq,
		}
		if best == nil {
			best = &cur
			return
		}
		if !cur.at.IsZero() && !best.at.IsZero() {
			if cur.at.After(best.at) || (cur.at.Equal(best.at) && cur.order > best.order) {
				best = &cur
			}
			return
		}
		if !cur.at.IsZero() && best.at.IsZero() {
			best = &cur
			return
		}
		if cur.at.IsZero() && best.at.IsZero() && cur.order > best.order {
			best = &cur
		}
	}

	for _, c := range comments {
		consider(c.ID, c.Author.Login, c.Body, c.CreatedAt)
	}
	for _, r := range reviews {
		if strings.TrimSpace(r.Body) == "" {
			continue
		}
		consider(r.ID, r.Author.Login, r.Body, r.SubmittedAt)
	}
	if best == nil {
		return "", "", ""
	}
	return best.id, best.author, best.body
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := text[:maxBytes]
	for len(cut) > 0 && (cut[len(cut)-1]&0xC0) == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && cut[len(cut)-1] >= 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
