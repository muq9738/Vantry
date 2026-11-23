// Package files provides workspace-confined directory listing, safe UTF-8 file
// previews, and @file fuzzy path search for Vantry's workbench and composer.
package files

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/internal/hostprocess"
)

const (
	// MaxPreviewBytes caps the returned text payload of ReadFilePreview at 256 KiB.
	MaxPreviewBytes = 256 * 1024

	defaultListLimit    = 500
	maxListLimit        = 5000
	defaultPreviewLines = 200
	maxPreviewLines     = 5000
	defaultSearchLimit  = 50
	maxSearchLimit      = 500

	maxPreviewScanBytes = 16 << 20
	maxGitLsOutputBytes = 8 << 20
	maxWalkCandidates   = 30000
	maxWalkDepth        = 32
	gitLsTimeout        = 10 * time.Second
)

// FileEntry describes a single directory or file returned by ListDirectory.
type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// FilePreview contains bounded line-sliced UTF-8 file content for rendering.
type FilePreview struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	TotalLines int    `json:"totalLines"`
	Size       int64  `json:"size"`
	Truncated  bool   `json:"truncated"`
}

// FileMatch represents a ranked candidate path returned by SearchFiles (@file).
type FileMatch struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Score     int    `json:"score"`
	MatchKind string `json:"matchKind"`
}

func resolveWorkspaceRoot(rootPath string) (string, error) {
	rootPath = strings.TrimSpace(rootPath)
	if rootPath == "" {
		return "", errors.New("工作区根路径不能为空")
	}
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", fmt.Errorf("解析工作区路径失败：%w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("解析工作区真实路径失败：%w", err)
	}
	info, err := os.Stat(realRoot)
	if err != nil {
		return "", fmt.Errorf("读取工作区根目录失败：%w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("工作区根路径不是目录：%s", rootPath)
	}
	return realRoot, nil
}

func isWithinRoot(realRoot, target string) bool {
	rel, err := filepath.Rel(realRoot, target)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func hasGitSegment(slashRel string) bool {
	for _, seg := range strings.Split(slashRel, "/") {
		if seg == ".git" {
			return true
		}
	}
	return false
}

func resolveRelativeTarget(realRoot, rawRel string, allowRoot bool) (cleanSlash string, realTarget string, err error) {
	if strings.IndexByte(rawRel, 0) >= 0 {
		return "", "", errors.New("路径包含非法空字节")
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(rawRel, "\\", "/"))
	normalized = strings.TrimPrefix(normalized, "./")
	if normalized == "" || normalized == "." {
		if !allowRoot {
			return "", "", errors.New("文件路径不能为空")
		}
		return "", realRoot, nil
	}
	if strings.HasPrefix(normalized, "/") || filepath.IsAbs(rawRel) {
		return "", "", errors.New("必须使用工作区内的相对路径")
	}
	cleanOS := filepath.Clean(filepath.FromSlash(normalized))
	if !filepath.IsLocal(cleanOS) {
		return "", "", errors.New("路径超出工作区根目录范围")
	}
	cleanSlash = filepath.ToSlash(cleanOS)
	if hasGitSegment(cleanSlash) {
		return "", "", errors.New("不允许访问 .git 内部路径")
	}

	joined := filepath.Join(realRoot, cleanOS)
	if !isWithinRoot(realRoot, joined) {
		return "", "", errors.New("路径超出工作区根目录范围")
	}
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", "", fmt.Errorf("解析目标路径失败：%w", err)
	}
	if !isWithinRoot(realRoot, resolved) {
		return "", "", errors.New("拒绝通过符号链接越界访问工作区外部路径")
	}
	relResolved, err := filepath.Rel(realRoot, resolved)
	if err != nil || hasGitSegment(filepath.ToSlash(relResolved)) {
		return "", "", errors.New("不允许访问 .git 内部路径")
	}
	return cleanSlash, resolved, nil
}

// ListDirectory lists immediate children of relDir within rootPath, confined via
// EvalSymlinks, skipping .git and escaping/broken symlinks, sorted directories
// first and then by name ascending.
func ListDirectory(ctx context.Context, rootPath, relDir string, limit int) ([]FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultListLimit
	} else if limit > maxListLimit {
		limit = maxListLimit
	}

	realRoot, err := resolveWorkspaceRoot(rootPath)
	if err != nil {
		return nil, err
	}
	cleanRel, realDir, err := resolveRelativeTarget(realRoot, relDir, true)
	if err != nil {
		return nil, err
	}
	dirInfo, err := os.Stat(realDir)
	if err != nil {
		return nil, err
	}
	if !dirInfo.IsDir() {
		return nil, fmt.Errorf("目标路径不是目录：%s", cleanRel)
	}

	rawEntries, err := os.ReadDir(realDir)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败：%w", err)
	}

	entries := make([]FileEntry, 0, len(rawEntries))
	for _, item := range rawEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := item.Name()
		if name == "" || name == ".git" {
			continue
		}

		entryFull := filepath.Join(realDir, name)
		resolvedEntry, err := filepath.EvalSymlinks(entryFull)
		if err != nil {
			// Skip dangling symlinks so listing a directory stays resilient.
			continue
		}
		if !isWithinRoot(realRoot, resolvedEntry) {
			// Never expose symlinks pointing outside the workspace root.
			continue
		}
		relToRoot, err := filepath.Rel(realRoot, resolvedEntry)
		if err != nil || hasGitSegment(filepath.ToSlash(relToRoot)) {
			continue
		}

		info, err := os.Stat(resolvedEntry)
		if err != nil {
			continue
		}
		isDir := info.IsDir()
		if !isDir && !info.Mode().IsRegular() {
			continue
		}

		entryRelPath := name
		if cleanRel != "" {
			entryRelPath = cleanRel + "/" + name
		}
		var size int64
		if !isDir {
			size = info.Size()
		}
		entries = append(entries, FileEntry{
			Name:  name,
			Path:  entryRelPath,
			IsDir: isDir,
			Size:  size,
		})
	}

	slices.SortFunc(entries, func(a, b FileEntry) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})

	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// ReadFilePreview safely reads a line range from a regular UTF-8 text file inside
// rootPath, rejecting path escapes, special files, NUL bytes, and invalid UTF-8,
// and capping output at 256 KiB.
func ReadFilePreview(ctx context.Context, rootPath, relPath string, startLine, maxLines int) (FilePreview, error) {
	if err := ctx.Err(); err != nil {
		return FilePreview{}, err
	}
	if startLine <= 0 {
		startLine = 1
	}
	if maxLines <= 0 {
		maxLines = defaultPreviewLines
	} else if maxLines > maxPreviewLines {
		maxLines = maxPreviewLines
	}

	realRoot, err := resolveWorkspaceRoot(rootPath)
	if err != nil {
		return FilePreview{}, err
	}
	cleanRel, realFile, err := resolveRelativeTarget(realRoot, relPath, false)
	if err != nil {
		return FilePreview{}, err
	}

	linfo, err := os.Lstat(realFile)
	if err != nil {
		return FilePreview{}, err
	}
	if !linfo.Mode().IsRegular() {
		return FilePreview{}, fmt.Errorf("非常规文件不支持预览：%s", cleanRel)
	}

	file, err := hostfs.OpenNoFollow(realFile, os.O_RDONLY, 0)
	if err != nil {
		return FilePreview{}, fmt.Errorf("打开文件失败：%w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return FilePreview{}, err
	}
	if !info.Mode().IsRegular() {
		return FilePreview{}, fmt.Errorf("非常规文件不支持预览：%s", cleanRel)
	}

	preview := FilePreview{
		Path:      cleanRel,
		StartLine: startLine,
		Size:      info.Size(),
	}
	if info.Size() == 0 {
		return preview, nil
	}

	endTargetLine := startLine + maxLines - 1
	var (
		readBuf       = make([]byte, 32*1024)
		utf8Carry     []byte
		lineCarry     bytes.Buffer
		outBuf        bytes.Buffer
		scannedBytes  int64
		currentLine   int
		firstSelected int
		lastSelected  int
		byteCapped    bool
		scanCapped    bool
	)

	emitLine := func(rawLine []byte) {
		currentLine++
		if currentLine < startLine || currentLine > endTargetLine || byteCapped {
			return
		}
		rawLine = bytes.TrimSuffix(rawLine, []byte{'\r'})
		needSep := firstSelected > 0
		sepLen := 0
		if needSep {
			sepLen = 1
		}
		remaining := MaxPreviewBytes - outBuf.Len()
		if remaining <= sepLen {
			byteCapped = true
			return
		}
		if needSep {
			_ = outBuf.WriteByte('\n')
			remaining--
		}
		if len(rawLine) > remaining {
			validSlice := trimValidUTF8Prefix(rawLine[:remaining])
			_, _ = outBuf.Write(validSlice)
			byteCapped = true
		} else {
			_, _ = outBuf.Write(rawLine)
		}
		if firstSelected == 0 {
			firstSelected = currentLine
		}
		lastSelected = currentLine
	}

	for {
		if err := ctx.Err(); err != nil {
			return FilePreview{}, err
		}
		toRead := len(readBuf)
		if rem := maxPreviewScanBytes - scannedBytes; rem <= 0 {
			scanCapped = true
			break
		} else if int64(toRead) > rem {
			toRead = int(rem)
		}

		n, readErr := file.Read(readBuf[:toRead])
		if n > 0 {
			scannedBytes += int64(n)
			chunk := readBuf[:n]
			if bytes.IndexByte(chunk, 0) >= 0 {
				return FilePreview{}, errors.New("检测到二进制文件（包含空字节），不支持文本预览")
			}

			atEOF := errors.Is(readErr, io.EOF)
			utf8Carry, err = validateStreamingUTF8(utf8Carry, chunk, atEOF)
			if err != nil {
				return FilePreview{}, err
			}

			data := chunk
			for len(data) > 0 {
				nl := bytes.IndexByte(data, '\n')
				if nl < 0 {
					// Cap retained single-line memory when outside preview window or already capped.
					if currentLine+1 >= startLine && currentLine+1 <= endTargetLine && !byteCapped && lineCarry.Len() <= MaxPreviewBytes {
						_, _ = lineCarry.Write(data)
					} else if lineCarry.Len() == 0 {
						// Remember that a non-empty trailing line without newline exists.
						_ = lineCarry.WriteByte(' ')
					}
					break
				}
				if lineCarry.Len() > 0 {
					if currentLine+1 >= startLine && currentLine+1 <= endTargetLine && !byteCapped && lineCarry.Len() <= MaxPreviewBytes {
						_, _ = lineCarry.Write(data[:nl])
					}
					emitLine(lineCarry.Bytes())
					lineCarry.Reset()
				} else {
					emitLine(data[:nl])
				}
				data = data[nl+1:]
			}
		}

		if errors.Is(readErr, io.EOF) {
			if len(utf8Carry) > 0 && !utf8.Valid(utf8Carry) {
				return FilePreview{}, errors.New("文件包含非法 UTF-8 编码，不支持文本预览")
			}
			if lineCarry.Len() > 0 {
				emitLine(lineCarry.Bytes())
				lineCarry.Reset()
			}
			break
		}
		if readErr != nil {
			return FilePreview{}, fmt.Errorf("读取文件失败：%w", readErr)
		}
	}

	if scanCapped && len(utf8Carry) > 0 {
		// Any trailing multi-byte sequence interrupted purely by scan cap is harmless;
		// full runes up to the cap have already been validated.
		utf8Carry = nil
	}
	if scanCapped && lineCarry.Len() > 0 {
		emitLine(lineCarry.Bytes())
	}

	preview.Content = outBuf.String()
	preview.TotalLines = currentLine
	preview.EndLine = lastSelected
	preview.Truncated = byteCapped || scanCapped || (lastSelected > 0 && lastSelected < currentLine) || (firstSelected == 0 && startLine <= currentLine)
	return preview, nil
}

func validateStreamingUTF8(carry, chunk []byte, atEOF bool) ([]byte, error) {
	var buf []byte
	if len(carry) > 0 {
		buf = make([]byte, len(carry)+len(chunk))
		copy(buf, carry)
		copy(buf[len(carry):], chunk)
	} else {
		buf = chunk
	}
	if atEOF {
		if !utf8.Valid(buf) {
			return nil, errors.New("文件包含非法 UTF-8 编码，不支持文本预览")
		}
		return nil, nil
	}

	// Check if the final 1..3 bytes begin an incomplete multi-byte UTF-8 sequence.
	var nextCarry []byte
	checkEnd := len(buf)
	minIdx := max(0, len(buf)-(utf8.UTFMax-1))
	for i := len(buf) - 1; i >= minIdx; i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				nextCarry = append([]byte(nil), buf[i:]...)
				checkEnd = i
			}
			break
		}
	}
	if !utf8.Valid(buf[:checkEnd]) {
		return nil, errors.New("文件包含非法 UTF-8 编码，不支持文本预览")
	}
	return nextCarry, nil
}

func trimValidUTF8Prefix(b []byte) []byte {
	for len(b) > 0 {
		if utf8.Valid(b) {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// SearchFiles searches workspace files for Composer @file autocomplete.
// Inside a Git worktree it prioritizes `git ls-files -co --exclude-standard -z`
// so .gitignore rules are respected; otherwise it performs a bounded directory walk.
// Matches are ranked: exact filename > filename/path prefix > path substring > fuzzy subsequence.
func SearchFiles(ctx context.Context, rootPath, query string, limit int) ([]FileMatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	} else if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	realRoot, err := resolveWorkspaceRoot(rootPath)
	if err != nil {
		return nil, err
	}

	candidates, usedGit, err := gitCandidateFiles(ctx, realRoot)
	if err != nil {
		return nil, err
	}
	if !usedGit {
		candidates, err = walkCandidateFiles(ctx, realRoot)
		if err != nil {
			return nil, err
		}
	}

	normalizedQuery := normalizeQuery(query)
	scored := make([]FileMatch, 0, min(len(candidates), 256))
	for _, relPath := range candidates {
		score, kind, ok := scoreFileCandidate(relPath, normalizedQuery)
		if !ok {
			continue
		}
		scored = append(scored, FileMatch{
			Path:      relPath,
			Name:      path.Base(relPath),
			Score:     score,
			MatchKind: kind,
		})
	}

	slices.SortFunc(scored, func(a, b FileMatch) int {
		if a.Score != b.Score {
			return cmp.Compare(b.Score, a.Score) // descending by score
		}
		if dA, dB := strings.Count(a.Path, "/"), strings.Count(b.Path, "/"); dA != dB {
			return cmp.Compare(dA, dB)
		}
		if len(a.Path) != len(b.Path) {
			return cmp.Compare(len(a.Path), len(b.Path))
		}
		return cmp.Compare(a.Path, b.Path)
	})

	results := make([]FileMatch, 0, min(limit, len(scored)))
	for _, item := range scored {
		if len(results) >= limit {
			break
		}
		if !usedGit || isSafeExistingWorkspaceFile(realRoot, item.Path) {
			results = append(results, item)
		}
	}
	return results, nil
}

func isSafeExistingWorkspaceFile(realRoot, slashRel string) bool {
	full := filepath.Join(realRoot, filepath.FromSlash(slashRel))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil || !isWithinRoot(realRoot, resolved) {
		return false
	}
	relToRoot, err := filepath.Rel(realRoot, resolved)
	if err != nil || hasGitSegment(filepath.ToSlash(relToRoot)) {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular()
}

func gitCandidateFiles(ctx context.Context, realRoot string) ([]string, bool, error) {
	gitCtx, cancel := context.WithTimeout(ctx, gitLsTimeout)
	defer cancel()

	cmd := hostprocess.NewContext(gitCtx, "git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-C", realRoot, "ls-files", "-co", "--exclude-standard", "-z")
	cmd.Env = workspace.GitEnvironment()
	defer cmd.Close()
	cmd.WaitDelay = time.Second

	var stdout, stderr bytes.Buffer
	limitWriter := &cappedByteWriter{dst: &stdout, limit: maxGitLsOutputBytes}
	errWriter := &cappedByteWriter{dst: &stderr, limit: 16 * 1024}
	cmd.Stdout = limitWriter
	cmd.Stderr = errWriter

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) || errors.Is(err, exec.ErrNotFound) {
			// Fallback cleanly when directory is not a Git repository or git is absent.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("执行 git ls-files 失败：%w", err)
	}

	raw := stdout.String()
	if limitWriter.truncated {
		if idx := strings.LastIndexByte(raw, 0); idx >= 0 {
			raw = raw[:idx+1]
		} else {
			raw = ""
		}
	}

	parts := strings.Split(raw, "\x00")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		slashPath := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
		if slashPath == "." || strings.HasPrefix(slashPath, "../") || slashPath == ".." || hasGitSegment(slashPath) {
			continue
		}
		if _, dup := seen[slashPath]; dup {
			continue
		}
		seen[slashPath] = struct{}{}
		out = append(out, slashPath)
		if len(out) >= maxWalkCandidates {
			break
		}
	}
	return out, true, nil
}

type cappedByteWriter struct {
	dst       *bytes.Buffer
	limit     int
	truncated bool
}

func (w *cappedByteWriter) Write(p []byte) (int, error) {
	n := len(p)
	rem := w.limit - w.dst.Len()
	if rem <= 0 {
		w.truncated = w.truncated || n > 0
		return n, nil
	}
	if len(p) > rem {
		p = p[:rem]
		w.truncated = true
	}
	_, _ = w.dst.Write(p)
	return n, nil
}

func walkCandidateFiles(ctx context.Context, realRoot string) ([]string, error) {
	type dirItem struct {
		realPath string
		relSlash string
		depth    int
	}
	queue := []dirItem{{realPath: realRoot, relSlash: "", depth: 0}}
	visitedDirs := map[string]struct{}{realRoot: {}}
	var files []string

	for len(queue) > 0 && len(files) < maxWalkCandidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur := queue[0]
		queue = queue[1:]

		entries, err := os.ReadDir(cur.realPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "" || name == ".git" {
				continue
			}
			full := filepath.Join(cur.realPath, name)
			resolved, err := filepath.EvalSymlinks(full)
			if err != nil || !isWithinRoot(realRoot, resolved) {
				continue
			}
			relToRoot, err := filepath.Rel(realRoot, resolved)
			if err != nil || hasGitSegment(filepath.ToSlash(relToRoot)) {
				continue
			}
			info, err := os.Stat(resolved)
			if err != nil {
				continue
			}
			childRel := name
			if cur.relSlash != "" {
				childRel = cur.relSlash + "/" + name
			}
			if info.IsDir() {
				if cur.depth+1 <= maxWalkDepth {
					if _, seen := visitedDirs[resolved]; !seen {
						visitedDirs[resolved] = struct{}{}
						queue = append(queue, dirItem{
							realPath: resolved,
							relSlash: childRel,
							depth:    cur.depth + 1,
						})
					}
				}
				continue
			}
			if info.Mode().IsRegular() {
				files = append(files, childRel)
				if len(files) >= maxWalkCandidates {
					break
				}
			}
		}
	}
	return files, nil
}

func normalizeQuery(q string) string {
	q = strings.TrimSpace(strings.ReplaceAll(q, "\\", "/"))
	q = strings.TrimPrefix(q, "@")
	q = strings.TrimSpace(q)
	q = strings.TrimPrefix(q, "./")
	return q
}

const (
	tierExactName  = 40000
	tierPrefixName = 30000
	tierSubstring  = 20000
	tierFuzzy      = 10000
)

func scoreFileCandidate(relPath, query string) (int, string, bool) {
	if query == "" {
		depthPenalty := min(strings.Count(relPath, "/")*50+len(relPath), 900)
		return 1000 - depthPenalty, "browse", true
	}

	tokens := strings.Fields(query)
	if len(tokens) == 1 {
		return scoreSingleToken(relPath, tokens[0])
	}

	totalScore := 0
	bestTier := 0
	bestKind := "fuzzy"
	lastBaseBonus := 0
	for i, tok := range tokens {
		s, kind, ok := scoreSingleToken(relPath, tok)
		if !ok {
			return 0, "", false
		}
		totalScore += s
		if s > bestTier {
			bestTier = s
			bestKind = kind
		}
		if i == len(tokens)-1 && s >= tierPrefixName {
			lastBaseBonus = 1200
		}
	}
	return totalScore/len(tokens) + lastBaseBonus, bestKind, true
}

func scoreSingleToken(relPath, tok string) (int, string, bool) {
	baseName := path.Base(relPath)
	lowerPath := strings.ToLower(relPath)
	lowerBase := strings.ToLower(baseName)
	lowerTok := strings.ToLower(tok)
	pathLenPenalty := min(len(relPath), 250)

	// Tier 4: Exact filename match (or exact full relative path match).
	if lowerBase == lowerTok || lowerPath == lowerTok {
		score := tierExactName + 1000 - pathLenPenalty
		if lowerPath == lowerTok {
			score += 800
		}
		if baseName == tok || relPath == tok {
			score += 400
		}
		return score, "exact_name", true
	}

	// Tier 3: Filename prefix match (or path prefix when query specifies a slash).
	if strings.HasPrefix(lowerBase, lowerTok) || (strings.Contains(lowerTok, "/") && strings.HasPrefix(lowerPath, lowerTok)) {
		score := tierPrefixName + 1000 - pathLenPenalty
		if strings.HasPrefix(lowerBase, lowerTok) {
			score += 800
			stem := strings.TrimSuffix(lowerBase, path.Ext(lowerBase))
			if stem == lowerTok {
				score += 1200
			}
			if strings.HasPrefix(baseName, tok) {
				score += 300
			}
			score += max(0, 200-len(baseName)*2)
		}
		return score, "prefix_name", true
	}

	// Tier 2: Contiguous substring in filename or relative path.
	if baseIdx := strings.Index(lowerBase, lowerTok); baseIdx >= 0 {
		score := tierSubstring + 2500 - pathLenPenalty - min(baseIdx*10, 300)
		if baseIdx > 0 && isBoundaryByte(lowerBase[baseIdx-1]) {
			score += 500
		}
		if strings.Contains(baseName, tok) {
			score += 200
		}
		return score, "substring", true
	}
	if pathIdx := strings.Index(lowerPath, lowerTok); pathIdx >= 0 {
		score := tierSubstring + 1000 - pathLenPenalty - min(pathIdx*3, 300)
		if pathIdx == 0 || isBoundaryByte(lowerPath[pathIdx-1]) {
			score += 400
		}
		if strings.Contains(relPath, tok) {
			score += 150
		}
		return score, "substring", true
	}

	// Tier 1: Ordered subsequence (fuzzy) match, preferring baseName over full path.
	if bonus, ok := fuzzySubsequenceScore(lowerBase, lowerTok); ok {
		score := tierFuzzy + 2000 + bonus - pathLenPenalty
		return clampScore(score, tierFuzzy+100, tierSubstring-1), "fuzzy", true
	}
	if bonus, ok := fuzzySubsequenceScore(lowerPath, lowerTok); ok {
		score := tierFuzzy + 800 + bonus - pathLenPenalty
		return clampScore(score, tierFuzzy+1, tierSubstring-1), "fuzzy", true
	}

	return 0, "", false
}

func isBoundaryByte(b byte) bool {
	return b == '/' || b == '_' || b == '-' || b == '.' || b == ' '
}

func fuzzySubsequenceScore(target, pattern string) (int, bool) {
	patRunes := []rune(pattern)
	if len(patRunes) == 0 {
		return 0, true
	}
	tgtRunes := []rune(target)
	if len(patRunes) > len(tgtRunes) {
		return 0, false
	}

	pIdx := 0
	firstMatch := -1
	prevMatch := -2
	bonus := 0

	for tIdx, r := range tgtRunes {
		if r != patRunes[pIdx] {
			continue
		}
		if firstMatch < 0 {
			firstMatch = tIdx
		}
		if tIdx == prevMatch+1 {
			bonus += 80
		}
		if tIdx == 0 || isBoundaryRune(tgtRunes[tIdx-1]) {
			bonus += 60
		}
		prevMatch = tIdx
		pIdx++
		if pIdx == len(patRunes) {
			span := prevMatch - firstMatch + 1
			gapPenalty := min((span-len(patRunes))*12, 500)
			return bonus - gapPenalty, true
		}
	}
	return 0, false
}

func isBoundaryRune(r rune) bool {
	return r == '/' || r == '_' || r == '-' || r == '.' || r == ' '
}

func clampScore(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
