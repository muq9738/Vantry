// Package sshconfig discovers OpenSSH client hosts from ~/.ssh/config
// without reading key material or exposing credentials. Probing lives in
// probe.go and runs one non-interactive ssh command per host.
package sshconfig

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/muq9738/Vantry/pkg/api"
)

const (
	configFileName   = "config"
	sshDirectoryName = ".ssh"
	defaultSSHPort   = 22
	maxIncludeDepth  = 6
	maxLineBytes     = 1 << 20
)

// gitForgeHosts are pure code-hosting endpoints: they belong to git
// credentials, not to the machines Vantry can host a Core on.
var gitForgeHosts = map[string]struct{}{
	"github.com":     {},
	"ssh.github.com": {},
	"gitlab.com":     {},
	"bitbucket.org":  {},
	"gitee.com":      {},
	"codeberg.org":   {},
}

// Options narrows or widens discovery. The zero value keeps git forges out
// and applies only directives written in a concrete Host block.
type Options struct {
	// IncludeGitForges keeps github.com and the other code hosts, for
	// callers that list every configured name.
	IncludeGitForges bool
}

// Discover reads ~/.ssh/config (and its Include files) from homeDir and
// returns one entry per concrete alias. An empty homeDir uses the current
// user's home directory. A missing config returns an empty slice and no
// error, just as OpenSSH treats it.
func Discover(homeDir string) ([]api.SSHHostInfo, error) {
	return DiscoverWith(homeDir, Options{})
}

// DiscoverWith is Discover with Options.
func DiscoverWith(homeDir string, opts Options) ([]api.SSHHostInfo, error) {
	home, err := resolveHome(homeDir)
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(home, sshDirectoryName)
	p := &parser{
		home:    home,
		sshDir:  sshDir,
		opts:    opts,
		visited: make(map[string]struct{}),
		seen:    make(map[string]struct{}),
		hosts:   []api.SSHHostInfo{},
	}
	block, err := p.parseFile(filepath.Join(sshDir, configFileName), 0, nil)
	if err != nil {
		return nil, err
	}
	p.flush(block)
	return p.hosts, nil
}

func resolveHome(homeDir string) (string, error) {
	if strings.TrimSpace(homeDir) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法确定用户主目录: %w", err)
		}
		return filepath.Clean(home), nil
	}
	home, err := filepath.Abs(homeDir)
	if err != nil {
		return "", fmt.Errorf("无效的主目录 %q: %w", homeDir, err)
	}
	return home, nil
}

// block is one Host section: aliases and the directives that follow it
// until the next Host or Match line. Directives of a block with no concrete
// alias (Host *, Match, code hosts) are discarded, so they never leak into
// the previous host.
type block struct {
	aliases []string
	// user and hostName are the destination the alias resolves to, which
	// the interface shows as user@host:port.
	user            string
	hostName        string
	port            int
	proxyJump       string
	forwardAgent    bool
	hasIdentityFile bool
	source          string
	ignored         bool
	flushed         bool
}

func (b *block) hasAliases() bool { return b != nil && !b.ignored && len(b.aliases) > 0 }

func (b *block) addPattern(pattern string, includeForges bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return
	}
	// Negations and wildcards match several hosts, so they name no host.
	if strings.ContainsAny(pattern, "*?!") {
		return
	}
	if !includeForges {
		if _, forge := gitForgeHosts[strings.ToLower(pattern)]; forge {
			return
		}
	}
	for _, existing := range b.aliases {
		if existing == pattern {
			return
		}
	}
	b.aliases = append(b.aliases, pattern)
}

type parser struct {
	home    string
	sshDir  string
	opts    Options
	visited map[string]struct{}
	seen    map[string]struct{}
	hosts   []api.SSHHostInfo
}

// parseFile parses path as if its lines were pasted at the Include
// directive, keeping the current Host block open across the boundary the
// way OpenSSH does. Missing include files are ignored; a missing root
// config is not an error either.
func (p *parser) parseFile(path string, depth int, current *block) (*block, error) {
	if depth > maxIncludeDepth {
		return current, nil
	}
	key := canonicalPath(path)
	if _, ok := p.visited[key]; ok {
		return current, nil
	}
	p.visited[key] = struct{}{}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return current, nil
		}
		if depth == 0 {
			return current, fmt.Errorf("读取 SSH 配置 %s 失败: %w", p.source(path), err)
		}
		// An unreadable include must not hide the hosts of the root file.
		return current, nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}
		name, value := splitDirective(line)
		switch strings.ToLower(name) {
		case "host":
			p.flush(current)
			current = &block{source: p.source(path)}
			for _, pattern := range splitFields(value) {
				current.addPattern(pattern, p.opts.IncludeGitForges)
			}
		case "match":
			// Match conditions have no stable host names: skip the section.
			p.flush(current)
			current = &block{source: p.source(path), ignored: true}
		case "include":
			// A top-level Include is valid and starts no Host block:
			// only a Match section suppresses it.
			if current != nil && current.ignored {
				continue
			}
			current, err = p.include(value, depth, current)
			if err != nil {
				return current, err
			}
		case "hostname":
			if !current.hasAliases() || value == "" || strings.EqualFold(value, "none") {
				continue
			}
			current.hostName = value
		case "user":
			if !current.hasAliases() || value == "" {
				continue
			}
			current.user = value
		case "port":
			if !current.hasAliases() {
				continue
			}
			if port, convErr := strconv.Atoi(value); convErr == nil && port > 0 && port <= 65535 && port != defaultSSHPort {
				current.port = port
			}
		case "proxyjump":
			if !current.hasAliases() {
				continue
			}
			if value != "" && !strings.EqualFold(value, "none") {
				current.proxyJump = value
			}
		case "forwardagent":
			if !current.hasAliases() {
				continue
			}
			current.forwardAgent = isYes(value)
		case "identityfile":
			if !current.hasAliases() {
				continue
			}
			if value != "" && !strings.EqualFold(value, "none") {
				current.hasIdentityFile = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if depth == 0 {
			return current, fmt.Errorf("读取 SSH 配置 %s 失败: %w", p.source(path), err)
		}
		return current, nil
	}
	return current, nil
}

func (p *parser) include(value string, depth int, current *block) (*block, error) {
	for _, pattern := range splitFields(value) {
		for _, path := range p.expandInclude(pattern) {
			next, err := p.parseFile(path, depth+1, current)
			if err != nil {
				return next, err
			}
			current = next
		}
	}
	return current, nil
}

// expandInclude resolves the patterns of an Include directive: "~/" against
// the home directory, relative patterns against ~/.ssh, then globs.
func (p *parser) expandInclude(pattern string) []string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil
	}
	switch {
	case pattern == "~":
		pattern = p.home
	case strings.HasPrefix(pattern, "~/"):
		pattern = filepath.Join(p.home, strings.TrimPrefix(pattern, "~/"))
	case filepath.IsAbs(pattern):
	default:
		pattern = filepath.Join(p.sshDir, pattern)
	}
	if !hasGlobMeta(pattern) {
		return []string{pattern}
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func hasGlobMeta(pattern string) bool { return strings.ContainsAny(pattern, "*?[") }

// flush records the block once. Blocks without a concrete alias never reach
// the result, and the first block of an alias wins, as in OpenSSH.
func (p *parser) flush(b *block) {
	if b == nil || b.flushed {
		return
	}
	b.flushed = true
	if len(b.aliases) == 0 {
		return
	}
	primary := b.aliases[0]
	if _, ok := p.seen[primary]; ok {
		return
	}
	p.seen[primary] = struct{}{}
	info := api.SSHHostInfo{
		Alias:           primary,
		User:            b.user,
		HostName:        b.hostName,
		Port:            b.port,
		ProxyJump:       b.proxyJump,
		ForwardAgent:    b.forwardAgent,
		HasIdentityFile: b.hasIdentityFile,
		SourceFile:      b.source,
	}
	if len(b.aliases) > 1 {
		info.Aliases = append([]string(nil), b.aliases...)
	}
	p.hosts = append(p.hosts, info)
}

// source shortens paths under the home directory to "~/..." for display.
func (p *parser) source(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	rel, err := filepath.Rel(p.home, abs)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return filepath.Join("~", rel)
	}
	return abs
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// stripComment drops a "#" comment at the start of a line or after
// whitespace, unless the "#" is inside a quoted value.
func stripComment(line string) string {
	inQuote := false
	for i, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '#' && !inQuote:
			if i == 0 || unicode.IsSpace(rune(line[i-1])) {
				return line[:i]
			}
		}
	}
	return line
}

// splitDirective splits "Key Value" and "Key=Value" at the first unquoted
// separator and returns the unquoted value.
func splitDirective(line string) (string, string) {
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '=' || c == ' ' || c == '\t':
			key := strings.TrimSpace(line[:i])
			rest := strings.TrimSpace(line[i:])
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
			return key, unquote(rest)
		}
	}
	return strings.TrimSpace(line), ""
}

// splitFields tokenizes a value, dropping surrounding quotes and collapsing
// whitespace, so multiple Include patterns and aliases survive one line.
func splitFields(value string) []string {
	var fields []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
	}
	for _, r := range value {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return fields
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func isYes(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "yes") || strings.EqualFold(strings.TrimSpace(value), "true")
}
