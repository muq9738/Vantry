package git

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const commitLogFormat = "%H%x1f%h%x1f%s%x1f%an%x1f%aI%x1f%D%x1f%P%x1e"

type CommitDetail struct {
	Commit    api.GitCommit `json:"commit"`
	Files     []api.GitFile `json:"files"`
	Diff      string        `json:"diff"`
	Truncated bool          `json:"truncated"`
	CommitExtras
}

func parseCommits(raw string) []api.GitCommit {
	commits := []api.GitCommit{}
	parts := strings.Split(raw, "\x1e")
	for _, record := range parts[:len(parts)-1] {
		fields := strings.Split(strings.TrimPrefix(record, "\n"), "\x1f")
		if len(fields) != 7 || fields[0] == "" {
			continue
		}
		authorTime, err := time.Parse(time.RFC3339, fields[4])
		if err != nil {
			continue
		}
		commit := api.GitCommit{
			ID: fields[0], ShortID: fields[1], Subject: fields[2], Author: fields[3], AuthorTime: authorTime,
			Parents: strings.Fields(fields[6]),
		}
		if fields[5] != "" {
			commit.Refs = strings.Split(fields[5], ", ")
		}
		commits = append(commits, commit)
	}
	return commits
}

func readCommitLog(ctx context.Context, root string, limit int, revisions, filter []string) ([]api.GitCommit, bool, error) {
	args := []string{"log", "-n", strconv.Itoa(limit), "--date-order", "--no-color", "--no-show-signature", "--decorate=short", "--format=" + commitLogFormat}
	args = append(args, filter...)
	args = append(args, revisions...)
	args = append(args, "--")
	result, err := execute(ctx, root, args...)
	if err != nil {
		return nil, false, err
	}
	return parseCommits(result.stdout.data.String()), result.stdout.truncated, nil
}

func resolveCommit(ctx context.Context, root, revision string) (string, error) {
	if strings.TrimSpace(revision) == "" || strings.HasPrefix(revision, "-") || strings.ContainsRune(revision, 0) {
		return "", errors.New("请选择有效的提交或分支引用")
	}
	id, err := command(ctx, root, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("提交引用不可用：%w", err)
	}
	return strings.TrimSpace(id), nil
}

// Log searches messages or author names, case-insensitively and literally.
// An empty branch includes the current history and remote-tracking branches;
// "*" includes all refs, and another value selects that revision's history.
func Log(ctx context.Context, path, query, branch string, limit int) ([]api.GitCommit, error) {
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	var revisions []string
	switch branch {
	case "*":
		revisions = []string{"--all"}
	case "":
		revisions = []string{"--remotes"}
		if id, err := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
			revisions = append(revisions, strings.TrimSpace(id))
		} else if !exitCode(err, 1) {
			return nil, err
		}
	default:
		id, err := resolveCommit(ctx, root, branch)
		if err != nil {
			return nil, err
		}
		revisions = []string{id}
	}
	if query == "" {
		commits, _, err := readCommitLog(ctx, root, limit, revisions, nil)
		return commits, err
	}
	if strings.ContainsRune(query, 0) {
		return nil, errors.New("搜索文字不能包含空字符")
	}
	commits, _, err := readCommitLog(ctx, root, limit, revisions, []string{"--grep=" + query, "-i", "--fixed-strings"})
	if err != nil {
		return nil, err
	}
	byAuthor, _, err := readCommitLog(ctx, root, limit, revisions, []string{"--author=" + query, "-i", "--fixed-strings"})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(commits))
	for _, commit := range commits {
		seen[commit.ID] = true
	}
	for _, commit := range byAuthor {
		if !seen[commit.ID] {
			commits = append(commits, commit)
			seen[commit.ID] = true
		}
	}
	sort.SliceStable(commits, func(i, j int) bool { return commits[i].AuthorTime.After(commits[j].AuthorTime) })
	if len(commits) > limit {
		commits = commits[:limit]
	}
	return commits, nil
}

func InspectCommit(ctx context.Context, path, revision string) (CommitDetail, error) {
	var detail CommitDetail
	ctx, cancel := withDefaultTimeout(ctx, defaultQueryTimeout)
	defer cancel()
	root, err := checkoutRoot(ctx, path)
	if err != nil {
		return detail, err
	}
	id, err := resolveCommit(ctx, root, revision)
	if err != nil {
		return detail, err
	}
	commits, truncated, err := readCommitLog(ctx, root, 1, []string{id}, nil)
	if err != nil {
		return detail, err
	}
	if len(commits) != 1 {
		return detail, errors.New("无法读取完整的提交元数据")
	}
	detail.Commit = commits[0]
	detail.Truncated = truncated
	files, err := execute(ctx, root, "diff-tree", "--no-commit-id", "--name-status", "-r", "-z", "-m", "--root", "--find-renames", "--no-ext-diff", "--no-textconv", id, "--")
	if err != nil {
		return detail, err
	}
	detail.Files = parseCommitFiles(files.stdout.data.String())
	detail.Truncated = detail.Truncated || files.stdout.truncated
	diff, err := execute(ctx, root, "show", "--format=", "--patch", "--diff-merges=first-parent", "--no-ext-diff", "--no-textconv", "--no-color", id, "--")
	if err != nil {
		return detail, err
	}
	detail.Diff = diff.stdout.String()
	detail.Truncated = detail.Truncated || diff.stdout.truncated
	detail.CommitExtras = commitExtras(ctx, root, id)
	return detail, nil
}

func parseCommitFiles(raw string) []api.GitFile {
	parts := strings.Split(raw, "\x00")
	files := []api.GitFile{}
	seen := make(map[string]bool)
	for i := 0; i+1 < len(parts)-1; {
		status := parts[i]
		name := parts[i+1]
		i += 2
		if status == "" || name == "" {
			continue
		}
		file := api.GitFile{Path: name, Index: status[:1], Worktree: " "}
		if strings.ContainsAny(file.Index, "RC") {
			if i >= len(parts)-1 {
				break
			}
			file.OriginalPath = name
			file.Path = parts[i]
			i++
		}
		if !seen[file.Path] {
			files = append(files, file)
			seen[file.Path] = true
		}
	}
	return files
}

// inspectHistory keeps the initial history query to one Git log invocation.
func inspectHistory(ctx context.Context, root string, state *api.GitState) error {
	commits, truncated, err := readCommitLog(ctx, root, 60, nil, nil)
	if err != nil {
		if !exitCode(err, 128) {
			return err
		}
		// An unborn branch is an ordinary empty history, even when other refs
		// exist. Preserve other errors instead of hiding a damaged repository.
		if _, verifyErr := command(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); !exitCode(verifyErr, 1) {
			return err
		}
		commits = []api.GitCommit{}
	}
	state.Commits = commits
	state.Truncated = state.Truncated || truncated
	state.Ahead, state.Behind, state.RemoteBranch, err = aheadBehind(ctx, root)
	if err != nil {
		return err
	}
	remotes, err := execute(ctx, root, "for-each-ref", "--format=%(refname:short)%00%(refname)", "refs/remotes/")
	if err != nil {
		return err
	}
	raw := remotes.stdout.data.String()
	if remotes.stdout.truncated {
		raw = raw[:strings.LastIndex(raw, "\n")+1]
		state.Truncated = true
	}
	state.RemoteBranches = []string{}
	for _, line := range strings.Split(raw, "\n") {
		name, ref, ok := strings.Cut(line, "\x00")
		if ok && name != "" && !strings.HasSuffix(ref, "/HEAD") {
			state.RemoteBranches = append(state.RemoteBranches, name)
		}
	}
	stashes, err := execute(ctx, root, "stash", "list", "--format=%gd%x00%gs")
	if err != nil {
		return err
	}
	raw = stashes.stdout.data.String()
	if stashes.stdout.truncated {
		raw = raw[:strings.LastIndex(raw, "\n")+1]
		state.Truncated = true
	}
	state.Stashes = []api.GitStash{}
	for _, line := range strings.Split(raw, "\n") {
		ref, subject, ok := strings.Cut(line, "\x00")
		if ok && stashRefPattern.MatchString(ref) {
			state.Stashes = append(state.Stashes, api.GitStash{Ref: ref, Branch: parseStashBranch(subject), Message: subject})
		}
	}
	return nil
}
