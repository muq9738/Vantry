package git

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/api"
)

// LogFilter narrows the history the Log page shows, as IDEA's log toolbar:
// text or hash, author, a path, and a recent period.
type LogFilter struct {
	Query  string `json:"query,omitempty"`
	Author string `json:"author,omitempty"`
	Path   string `json:"path,omitempty"`
	Since  string `json:"since,omitempty"` // "1d", "7d", "30d" or "365d"
	Branch string `json:"branch,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

var sinceDays = map[string]int{"1d": 1, "7d": 7, "30d": 30, "365d": 365}

// LogFiltered is Log with author, path and period filters. A query that
// looks like a commit hash also matches that commit.
func LogFiltered(ctx context.Context, path string, filter LogFilter) ([]api.GitCommit, error) {
	if filter.Author == "" && filter.Path == "" && filter.Since == "" {
		return Log(ctx, path, filter.Query, filter.Branch, filter.Limit)
	}
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	revisions := []string{"--remotes", "HEAD"}
	switch filter.Branch {
	case "*":
		revisions = []string{"--all"}
	case "":
		if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
			if !exitCode(err, 1) {
				return nil, err
			}
			revisions = []string{"--remotes"}
		}
	default:
		id, err := resolveCommit(ctx, root, filter.Branch)
		if err != nil {
			return nil, err
		}
		revisions = []string{id}
	}
	var args []string
	for _, value := range []string{filter.Query, filter.Author, filter.Path} {
		if strings.ContainsRune(value, 0) || strings.HasPrefix(value, "-") {
			return nil, errors.New("筛选条件无效")
		}
	}
	if filter.Query != "" {
		args = append(args, "--grep="+filter.Query, "-i", "--fixed-strings")
	}
	if filter.Author != "" {
		args = append(args, "--author="+filter.Author)
	}
	if days, ok := sinceDays[filter.Since]; ok {
		args = append(args, "--since="+strconv.Itoa(days)+".days.ago")
	}
	if filter.Path != "" {
		if err := checkPath(root, filter.Path); err != nil {
			return nil, err
		}
		revisions = append(revisions, "--", filter.Path)
		commits, _, err := readCommitLogPaths(ctx, root, limit, revisions, args)
		return commits, err
	}
	commits, _, err := readCommitLog(ctx, root, limit, revisions, args)
	return commits, err
}

// readCommitLogPaths is readCommitLog whose revisions already end with
// "-- path".
func readCommitLogPaths(ctx context.Context, root string, limit int, revisions, filter []string) ([]api.GitCommit, bool, error) {
	args := []string{"log", "-n", strconv.Itoa(limit), "--date-order", "--no-color", "--no-show-signature", "--decorate=short", "--format=" + commitLogFormat}
	args = append(args, filter...)
	args = append(args, revisions...)
	result, err := execute(ctx, root, args...)
	if err != nil {
		return nil, false, err
	}
	return parseCommits(result.stdout.data.String()), result.stdout.truncated, nil
}

// CommitExtras are the facts IDEA's commit details show beside the files:
// the full message, the author's address, and the branches holding it.
type CommitExtras struct {
	Body        string   `json:"body,omitempty"`
	AuthorEmail string   `json:"authorEmail,omitempty"`
	Branches    []string `json:"branches,omitempty"`
	IsHead      bool     `json:"isHead,omitempty"`
}

func commitExtras(ctx context.Context, root, id string) CommitExtras {
	var extras CommitExtras
	if out, err := command(ctx, root, "log", "-1", "--no-color", "--format=%ae%x1f%B", id, "--"); err == nil {
		email, body, _ := strings.Cut(out, "\x1f")
		extras.AuthorEmail, extras.Body = strings.TrimSpace(email), strings.TrimSpace(body)
	}
	if out, err := command(ctx, root, "for-each-ref", "--count=40", "--contains="+id, "--format=%(refname:short)", "refs/heads/", "refs/remotes/"); err == nil {
		for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
			if name != "" && !strings.HasSuffix(name, "/HEAD") {
				extras.Branches = append(extras.Branches, name)
			}
		}
	}
	if head, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		extras.IsHead = strings.TrimSpace(head) == id
	}
	return extras
}

// commitOpInput carries the name a tag or branch takes, or a new message.
type commitOpInput struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
}

// CommitOperation runs one of the Log page's commit actions on revision:
// cherry-pick, revert, reset-soft/mixed/hard, checkout (detached), branch,
// tag, patch (returned as text), undo (HEAD only) and reword (HEAD only).
// Every change holds the checkout lock and refuses a paused operation.
func CommitOperation(ctx context.Context, path, revision, op string, raw json.RawMessage) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultLocalOpTimeout)
	defer cancel()
	var input commitOpInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return "", errors.New("提交操作参数无效")
		}
	}
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return "", err
	}
	id, err := resolveCommit(ctx, root, revision)
	if err != nil {
		return "", err
	}
	if op == "patch" {
		result, err := execute(ctx, root, "format-patch", "-1", "--stdout", "--no-color", id)
		return result.stdout.String(), err
	}
	lock, err := lockCheckout(ctx, root)
	if err != nil {
		return "", err
	}
	defer workspace.Unlock(lock)
	if err := requireNoPendingOperation(ctx, root); err != nil {
		return "", err
	}
	head := ""
	if out, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		head = strings.TrimSpace(out)
	}
	var args []string
	switch op {
	case "cherry-pick":
		args = []string{"cherry-pick", "--allow-empty", id}
	case "revert":
		args = []string{"revert", "--no-edit", id}
	case "reset-soft", "reset-mixed", "reset-hard":
		args = []string{"reset", "--" + strings.TrimPrefix(op, "reset-"), id}
	case "checkout":
		args = []string{"switch", "--detach", "--no-overwrite-ignore", id}
	case "branch", "tag":
		name := strings.TrimSpace(input.Name)
		if op == "branch" {
			if err := validateBranchName(ctx, root, name); err != nil {
				return "", err
			}
			args = []string{"branch", "--", name, id}
		} else {
			if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\r\n~^:?*[\\") {
				return "", errors.New("请输入有效的标签名")
			}
			args = []string{"tag", "--", name, id}
		}
	case "undo":
		if id != head {
			return "", errors.New("只能撤销当前分支的最新提交")
		}
		if _, err := command(ctx, root, "rev-parse", "--verify", "--quiet", id+"^"); err != nil {
			return "", errors.New("首个提交不能撤销")
		}
		args = []string{"reset", "--soft", id + "^"}
	case "reword":
		if id != head {
			return "", errors.New("只能编辑当前分支最新提交的说明")
		}
		message := strings.TrimSpace(input.Message)
		if message == "" {
			return "", errors.New("提交说明不能为空")
		}
		args = []string{"commit", "--amend", "--only", "--allow-empty", "-m", message}
	default:
		return "", errors.New("未知的提交操作")
	}
	result, err := execute(ctx, root, args...)
	return result.text(false), err
}
