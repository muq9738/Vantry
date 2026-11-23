// Package environments keeps the catalog of remote Vantry hosts this desktop
// may link to: the SSH target, the Core socket on it, and where its default
// workspace lives. The catalog is one JSON document in the user's private data
// directory, written atomically with owner-only permissions.
package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/muq9738/Vantry/internal/adapters/transport/sshrelay"
)

const (
	// FileName is the catalog document inside the data directory.
	FileName = "environments.json"
	// DefaultRemoteSocket is the socket a remote Core listens on under the
	// remote user's home. An entry that leaves SocketPath empty uses it.
	DefaultRemoteSocket = "~/.vantry/core.sock"
	// DefaultProbeTimeout bounds one reachability check, child process
	// included, so a silent host cannot hold the panel open.
	DefaultProbeTimeout = 6 * time.Second
	// probeWorkers is how many hosts are checked at once.
	probeWorkers = 4

	fileVersion = 1
)

// EnvironmentEntry is one linked machine.
type EnvironmentEntry struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"` // the alias the UI shows, e.g. "MacBook"
	Host             string    `json:"host"` // user@host or an SSH Host alias
	SocketPath       string    `json:"socketPath,omitempty"`
	DefaultWorkspace string    `json:"defaultWorkspace,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	LastConnectedAt  time.Time `json:"lastConnectedAt,omitzero"` // zero: never connected
}

// Socket is the remote Core socket this entry declares, which may start with
// "~" for the remote home.
func (e EnvironmentEntry) Socket() string {
	if socket := strings.TrimSpace(e.SocketPath); socket != "" {
		return socket
	}
	return DefaultRemoteSocket
}

// ResolveSocket returns an absolute remote socket path. SSH stream forwarding
// does not expand "~" (the remote shell does, but a forward is not a shell),
// so whoever opens a forward passes the remote home it learned for this host.
func (e EnvironmentEntry) ResolveSocket(remoteHome string) (string, error) {
	socket := e.Socket()
	if !strings.HasPrefix(socket, "~") {
		return socket, nil
	}
	home := strings.TrimSpace(remoteHome)
	if home == "" {
		return "", fmt.Errorf("远端 socket %s 需要远端 home 目录才能展开", socket)
	}
	return expandHomeDir(home, socket)
}

// ProbeResult is one reachability answer.
type ProbeResult struct {
	Reachable bool      `json:"reachable"`
	LatencyMs int64     `json:"latencyMs"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// EnvironmentStatus pairs an entry with its latest probe.
type EnvironmentStatus struct {
	Environment EnvironmentEntry `json:"environment"`
	ProbeResult
}

// Catalog owns dataDir/environments.json. All methods are safe for concurrent
// use; the slice a caller receives is a copy.
type Catalog struct {
	path string

	mu      sync.Mutex
	entries []EnvironmentEntry
	loaded  bool
}

// New opens (not yet reads) the catalog under dataDir.
func New(dataDir string) (*Catalog, error) {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		return nil, errors.New("环境目录需要 Vantry 数据目录")
	}
	return &Catalog{path: filepath.Join(dir, FileName)}, nil
}

// Path is the catalog document.
func (c *Catalog) Path() string { return c.path }

type catalogFile struct {
	Version      int                `json:"version"`
	Environments []EnvironmentEntry `json:"environments"`
}

// Load returns every entry. A missing document is an empty catalog; an
// unreadable or invalid one is an error so the UI can say why it is empty.
func (c *Catalog) Load() ([]EnvironmentEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadLocked()
}

func (c *Catalog) loadLocked() ([]EnvironmentEntry, error) {
	if c.loaded {
		return copyEntries(c.entries), nil
	}
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		c.entries, c.loaded = nil, true
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取环境目录：%w", err)
	}
	var doc catalogFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("环境目录不是有效的 JSON：%w", err)
	}
	if doc.Version > fileVersion {
		return nil, fmt.Errorf("环境目录版本 %d 高于本客户端支持的 %d，请升级 Vantry", doc.Version, fileVersion)
	}
	entries := make([]EnvironmentEntry, 0, len(doc.Environments))
	seen := make(map[string]bool, len(doc.Environments))
	for _, raw := range doc.Environments {
		entry := normalizeEntry(raw)
		if entry.ID == "" {
			return nil, errors.New("环境目录包含缺少 ID 的条目")
		}
		if seen[entry.ID] {
			return nil, fmt.Errorf("环境目录包含重复的 ID：%s", entry.ID)
		}
		if err := ValidateEntry(entry); err != nil {
			return nil, fmt.Errorf("环境目录中的 %s 无效：%w", entry.Name, err)
		}
		seen[entry.ID] = true
		entries = append(entries, entry)
	}
	c.entries, c.loaded = entries, true
	return copyEntries(entries), nil
}

// Save writes a whole catalog, in the order given, after validating each entry.
func (c *Catalog) Save(entries []EnvironmentEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	normalized := make([]EnvironmentEntry, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, raw := range entries {
		entry := normalizeEntry(raw)
		if entry.ID == "" {
			return errors.New("保存环境前需要给每个环境一个 ID")
		}
		if seen[entry.ID] {
			return fmt.Errorf("环境 ID 重复：%s", entry.ID)
		}
		if err := ValidateEntry(entry); err != nil {
			return fmt.Errorf("环境 %s 无效：%w", entry.Name, err)
		}
		seen[entry.ID] = true
		normalized = append(normalized, entry)
	}
	if err := c.writeLocked(normalized); err != nil {
		return err
	}
	c.entries, c.loaded = normalized, true
	return nil
}

// Upsert adds an entry or replaces the one with the same ID. A new entry
// without an ID gets one derived from its name or host; a replacement keeps
// the creation time and last connection it already had.
func (c *Catalog) Upsert(entry EnvironmentEntry) (EnvironmentEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.loadLocked()
	if err != nil {
		return EnvironmentEntry{}, err
	}
	next := normalizeEntry(entry)
	if next.ID == "" {
		next.ID = uniqueID(entries, slugID(next.Name, next.Host))
	}
	for i, existing := range entries {
		if existing.ID != next.ID {
			continue
		}
		if next.CreatedAt.IsZero() {
			next.CreatedAt = existing.CreatedAt
		}
		if next.LastConnectedAt.IsZero() {
			next.LastConnectedAt = existing.LastConnectedAt
		}
		if err := ValidateEntry(next); err != nil {
			return EnvironmentEntry{}, err
		}
		entries[i] = next
		if err := c.writeLocked(entries); err != nil {
			return EnvironmentEntry{}, err
		}
		c.entries = entries
		return next, nil
	}
	if next.CreatedAt.IsZero() {
		next.CreatedAt = time.Now().UTC()
	}
	if err := ValidateEntry(next); err != nil {
		return EnvironmentEntry{}, err
	}
	entries = append(entries, next)
	if err := c.writeLocked(entries); err != nil {
		return EnvironmentEntry{}, err
	}
	c.entries = entries
	return next, nil
}

// Remove deletes one entry by ID.
func (c *Catalog) Remove(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.loadLocked()
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(id)
	for i, entry := range entries {
		if entry.ID != trimmed {
			continue
		}
		rest := append(entries[:i:i], entries[i+1:]...)
		if err := c.writeLocked(rest); err != nil {
			return err
		}
		c.entries = rest
		return nil
	}
	return fmt.Errorf("环境不存在：%s", trimmed)
}

// TouchConnected records a successful connection. A zero time means now.
func (c *Catalog) TouchConnected(id string, at time.Time) (EnvironmentEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := c.loadLocked()
	if err != nil {
		return EnvironmentEntry{}, err
	}
	trimmed := strings.TrimSpace(id)
	if at.IsZero() {
		at = time.Now().UTC()
	}
	for i, entry := range entries {
		if entry.ID != trimmed {
			continue
		}
		entries[i].LastConnectedAt = at.UTC()
		if err := c.writeLocked(entries); err != nil {
			return EnvironmentEntry{}, err
		}
		c.entries = entries
		return entries[i], nil
	}
	return EnvironmentEntry{}, fmt.Errorf("环境不存在：%s", trimmed)
}

// writeLocked replaces the document: a temporary file in the same directory,
// fsynced, renamed over the old one, so a reader never sees half a catalog.
func (c *Catalog) writeLocked(entries []EnvironmentEntry) error {
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建环境目录：%w", err)
	}
	data, err := json.MarshalIndent(catalogFile{Version: fileVersion, Environments: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码环境目录：%w", err)
	}
	file, err := os.CreateTemp(dir, ".environments-*.tmp")
	if err != nil {
		return fmt.Errorf("写入环境目录：%w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("设置环境目录权限：%w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入环境目录：%w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入环境目录：%w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("写入环境目录：%w", err)
	}
	if err := os.Rename(name, c.path); err != nil {
		return fmt.Errorf("替换环境目录：%w", err)
	}
	return nil
}

// ProbeAll checks every entry at once, at most probeWorkers at a time, and
// returns the statuses in catalog order. probeFn nil uses DefaultProbe.
func (c *Catalog) ProbeAll(ctx context.Context, probeFn func(context.Context, EnvironmentEntry) ProbeResult) []EnvironmentStatus {
	if ctx == nil {
		ctx = context.Background()
	}
	entries, _ := c.Load()
	if len(entries) == 0 {
		return nil
	}
	if probeFn == nil {
		probeFn = DefaultProbe
	}
	statuses := make([]EnvironmentStatus, len(entries))
	workers := make(chan struct{}, probeWorkers)
	var wg sync.WaitGroup
	for i, entry := range entries {
		statuses[i].Environment = entry
		statuses[i].CheckedAt = time.Now().UTC()
		wg.Add(1)
		go func(i int, entry EnvironmentEntry) {
			defer wg.Done()
			workers <- struct{}{}
			defer func() { <-workers }()
			probeCtx, cancel := context.WithTimeout(ctx, DefaultProbeTimeout)
			defer cancel()
			statuses[i].ProbeResult = runProbe(probeCtx, probeFn, entry)
		}(i, entry)
	}
	wg.Wait()
	return statuses
}

// runProbe isolates a probe: a panic or an ignored context becomes a status
// the panel can show instead of taking the window down.
func runProbe(ctx context.Context, probeFn func(context.Context, EnvironmentEntry) ProbeResult, entry EnvironmentEntry) (result ProbeResult) {
	defer func() {
		if r := recover(); r != nil {
			result = ProbeResult{Detail: fmt.Sprintf("探测环境时出错：%v", r), CheckedAt: time.Now().UTC()}
		}
	}()
	started := time.Now()
	result = probeFn(ctx, entry)
	result.CheckedAt = time.Now().UTC()
	if result.LatencyMs <= 0 {
		result.LatencyMs = time.Since(started).Milliseconds()
	}
	return result
}

// DefaultProbe asks a host, without a password and without a terminal, whether
// the Core socket exists. The remote shell expands a leading "~".
func DefaultProbe(ctx context.Context, entry EnvironmentEntry) ProbeResult {
	result := ProbeResult{CheckedAt: time.Now().UTC()}
	started := time.Now()
	if err := ValidateEntry(entry); err != nil {
		result.Detail = err.Error()
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	command := exec.CommandContext(ctx, "ssh", probeArgs(entry)...)
	var stderr limitedTail
	command.Stderr = &stderr
	command.WaitDelay = time.Second
	err := command.Run()
	result.LatencyMs = time.Since(started).Milliseconds()
	if err == nil {
		result.Reachable = true
		result.Detail = "远端 Core socket 存在"
		return result
	}
	detail := stderr.String()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Detail = "探测超时"
	case detail != "":
		result.Detail = detail
	default:
		result.Detail = err.Error()
	}
	return result
}

// probeArgs is the ssh invocation DefaultProbe runs: one non-interactive
// command, no terminal, no prompting, and the host only after "--" so an alias
// that starts with "-" cannot become an option.
func probeArgs(entry EnvironmentEntry) []string {
	return []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ConnectionAttempts=1",
		"-o", "StrictHostKeyChecking=yes",
		"--", strings.TrimSpace(entry.Host),
		probeCommand(entry.Socket()),
	}
}

// probeCommand tests the socket through the remote shell.
func probeCommand(socket string) string { return "test -S " + shellQuote(socket) }

// shellQuote keeps an ordinary path bare and single-quotes anything else, so a
// path with a space cannot become two commands.
func shellQuote(s string) string {
	safe := s != "" && strings.IndexFunc(s, func(r rune) bool { return !shellSafeRune(r) }) < 0
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellSafeRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._-~:@%+=,", r)
}

// ValidateEntry applies the same boundaries a forward does: no option-shaped
// host, no whitespace or control characters, and a socket that is absolute
// (or the documented "~" form) with no colon.
func ValidateEntry(entry EnvironmentEntry) error {
	if err := validateText(entry.Name, 80, "环境名称"); err != nil {
		return err
	}
	if entry.ID != "" {
		if len(entry.ID) > 64 {
			return errors.New("环境 ID 过长")
		}
		for _, r := range entry.ID {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-", r) {
				return fmt.Errorf("环境 ID 只能使用字母、数字、点、下划线和连字符：%s", entry.ID)
			}
		}
	}
	host := strings.TrimSpace(entry.Host)
	if host == "" {
		return errors.New("请输入 SSH 主机名或 user@host")
	}
	if host != entry.Host {
		return errors.New("SSH 主机名首尾不能有空白")
	}
	socket, err := validationSocket(entry.Socket())
	if err != nil {
		return err
	}
	if err := sshrelay.Validate(host, socket); err != nil {
		return err
	}
	if workspace := entry.DefaultWorkspace; workspace != "" {
		if workspace != strings.TrimSpace(workspace) {
			return errors.New("默认工作区首尾不能有空白")
		}
		if err := validateText(workspace, 1024, "默认工作区"); err != nil {
			return err
		}
		if !filepath.IsAbs(workspace) && !strings.HasPrefix(workspace, "~/") {
			return errors.New("默认工作区必须是绝对路径，或以 ~/ 开头的远端路径")
		}
		if strings.ContainsAny(workspace, ":\r\n\x00") {
			return errors.New("默认工作区不能含冒号或换行")
		}
	}
	return nil
}

// validationSocket turns a declared socket into the absolute form sshrelay
// checks, so the catalog and a real forward agree on what is allowed. The home
// directory used for the "~" form is a placeholder: only the shape is checked.
func validationSocket(socket string) (string, error) {
	if strings.HasPrefix(socket, "~/") {
		return expandHomeDir("/home/user", socket)
	}
	if strings.HasPrefix(socket, "~") {
		return "", errors.New("socket 只能是绝对路径，或以 ~/ 开头的远端路径")
	}
	return socket, nil
}

func expandHomeDir(home, socket string) (string, error) {
	rest := strings.TrimPrefix(socket, "~")
	if !strings.HasPrefix(rest, "/") || len(rest) < 2 {
		return "", fmt.Errorf("socket 路径不完整：%s", socket)
	}
	return strings.TrimSuffix(home, "/") + rest, nil
}

func validateText(value string, limit int, what string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s不能为空", what)
	}
	if trimmed != value {
		return fmt.Errorf("%s首尾不能有空白", what)
	}
	if utf8Len(value) > limit {
		return fmt.Errorf("%s超过 %d 个字符", what, limit)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s不能含换行或控制字符", what)
		}
	}
	return nil
}

func utf8Len(s string) int { return len([]rune(s)) }

func normalizeEntry(entry EnvironmentEntry) EnvironmentEntry {
	entry.ID = strings.TrimSpace(entry.ID)
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Host = strings.TrimSpace(entry.Host)
	entry.SocketPath = strings.TrimSpace(entry.SocketPath)
	entry.DefaultWorkspace = strings.TrimSpace(entry.DefaultWorkspace)
	if entry.SocketPath == DefaultRemoteSocket {
		entry.SocketPath = "" // the documented default stays implicit
	}
	return entry
}

func copyEntries(entries []EnvironmentEntry) []EnvironmentEntry {
	if len(entries) == 0 {
		return nil
	}
	return append([]EnvironmentEntry(nil), entries...)
}

// slugID names a new entry after its alias, then its host. It is a readable
// identifier, not a derived key: nothing here is computed from content.
func slugID(name, host string) string {
	for _, candidate := range []string{name, host} {
		var b strings.Builder
		lastDash := false
		for _, r := range strings.ToLower(candidate) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
				lastDash = false
			case r >= 0x80:
				b.WriteRune(r) // keep non-Latin names readable
				lastDash = false
			case !lastDash:
				b.WriteByte('-')
				lastDash = true
			}
		}
		slug := strings.Trim(b.String(), "-")
		if slug != "" {
			if len([]rune(slug)) > 48 {
				slug = string([]rune(slug)[:48])
			}
			return slug
		}
	}
	return "environment"
}

func uniqueID(entries []EnvironmentEntry, base string) string {
	taken := make(map[string]bool, len(entries))
	for _, entry := range entries {
		taken[entry.ID] = true
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := base + "-" + strconv.Itoa(i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// SortByName orders entries for a panel listing, oldest first within a name.
func SortByName(entries []EnvironmentEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if a != b {
			return a < b
		}
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
}

// limitedTail bounds what a failing ssh can push into memory, and keeps
// control characters out of the panel that shows it.
type limitedTail struct {
	mu   sync.Mutex
	data []byte
}

const tailLimit = 2 << 10

func (t *limitedTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= tailLimit {
		t.data = append(t.data[:0], p[len(p)-tailLimit:]...)
		return len(p), nil
	}
	if keep := tailLimit - len(p); len(t.data) > keep {
		t.data = append(t.data[:0], t.data[len(t.data)-keep:]...)
	}
	t.data = append(t.data, p...)
	return len(p), nil
}

func (t *limitedTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(string(t.data), "")))
}
