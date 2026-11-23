package t3compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/muq9738/Vantry/pkg/api"
)

type rpcRequest struct {
	Tag       string          `json:"_tag"`
	ID        json.RawMessage `json:"id"`
	RequestID json.RawMessage `json:"requestId"`
	Method    string          `json:"tag"`
	Payload   json.RawMessage `json:"payload"`
}
type rpcSocket struct {
	server   *Server
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	writes   sync.Mutex
	mu       sync.Mutex
	requests map[string]context.CancelFunc
	scopes   string
	wg       sync.WaitGroup
}

func queryKeys(req *http.Request) string {
	query := req.URL.Query()
	if len(query) == 0 {
		return ""
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (s *Server) socket(w http.ResponseWriter, req *http.Request) {
	key := strings.TrimSpace(req.URL.Query().Get("wsTicket"))
	qKeys := queryKeys(req)
	var (
		client     Client
		token      string
		authSource string
		ok         bool
	)
	if key != "" {
		authSource = "wsTicket"
		s.mu.Lock()
		tkt, exists := s.tickets[key]
		delete(s.tickets, key)
		s.mu.Unlock()
		if !exists || !time.Now().Before(tkt.expires) {
			reason := "missing_ticket"
			if exists {
				reason = "expired_ticket"
			}
			log.Printf("t3compat ws auth rejected source=%s reason=%s queryKeys=%q ua=%q", authSource, reason, qKeys, req.UserAgent())
			authError(w)
			return
		}
		client, ok = s.authenticated(tkt.token)
		if !ok {
			log.Printf("t3compat ws auth rejected source=%s reason=invalid_session queryKeys=%q ua=%q", authSource, qKeys, req.UserAgent())
			authError(w)
			return
		}
		token = tkt.token
	} else {
		bearer, method := extractRequestToken(req)
		authSource = nonempty(method, "none")
		client, ok = s.authenticated(bearer)
		if !ok {
			log.Printf("t3compat ws auth rejected source=%s reason=unauthorized queryKeys=%q ua=%q", authSource, qKeys, req.UserAgent())
			authError(w)
			return
		}
		token = bearer
	}
	conn, err := (&websocket.Upgrader{EnableCompression: true, CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
	if err != nil {
		log.Printf("t3compat ws upgrade error source=%s sid=%s err=%v", authSource, client.ID, err)
		return
	}
	log.Printf("t3compat ws connected source=%s sid=%s queryKeys=%q ua=%q", authSource, client.ID, qKeys, req.UserAgent())
	_ = conn.SetCompressionLevel(1)
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(req.Context())
	socket := &rpcSocket{server: s, conn: conn, ctx: ctx, cancel: cancel, requests: map[string]context.CancelFunc{}, scopes: nonempty(client.Scopes, scopes)}
	s.mu.Lock()
	s.sockets[conn] = token
	s.mu.Unlock()
	defer func() {
		cancel()
		_ = conn.Close()
		socket.wg.Wait()
		s.mu.Lock()
		delete(s.sockets, conn)
		s.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("t3compat ws disconnected sid=%s err=%v", client.ID, err)
			return
		}
		// Effect's JSON serializer sends individual messages; tolerate a batch too.
		var batch []json.RawMessage
		if len(data) > 0 && data[0] == '[' {
			if err := json.Unmarshal(data, &batch); err != nil {
				log.Printf("t3compat ws batch decode error sid=%s err=%v", client.ID, err)
				return
			}
		} else {
			batch = []json.RawMessage{data}
		}
		for _, raw := range batch {
			var request rpcRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				log.Printf("t3compat ws frame decode error sid=%s err=%v", client.ID, err)
				return
			}
			switch request.Tag {
			case "Ping":
				if socket.send(Object{"_tag": "Pong"}) != nil {
					return
				}
			case "Ack", "Pong", "Eof", "ResendPreviousSeq":
			case "Interrupt":
				socket.mu.Lock()
				cancel := socket.requests[string(request.RequestID)]
				socket.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			case "Request":
				if len(request.ID) == 0 || request.Method == "" {
					log.Printf("t3compat ws invalid request sid=%s method=%q", client.ID, request.Method)
					continue
				}
				id := string(request.ID)
				socket.mu.Lock()
				_, duplicate := socket.requests[id]
				if duplicate || len(socket.requests) >= 256 {
					socket.mu.Unlock()
					log.Printf("t3compat ws request rejected sid=%s id=%s method=%q duplicate=%t active=%d", client.ID, id, request.Method, duplicate, len(socket.requests))
					socket.exit(request.ID, nil, errors.New("重复或超限的 RPC 请求"))
					continue
				}
				life, stop := context.WithCancel(ctx)
				socket.requests[id] = stop
				socket.mu.Unlock()
				socket.wg.Add(1)
				go func() {
					defer socket.wg.Done()
					defer stop()
					defer func() { socket.mu.Lock(); delete(socket.requests, id); socket.mu.Unlock() }()
					socket.request(life, request)
				}()
			default:
				log.Printf("t3compat ws unknown frame tag=%q sid=%s", request.Tag, client.ID)
				continue
			}
		}
	}
}
func (r *rpcSocket) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r.writes.Lock()
	defer r.writes.Unlock()
	_ = r.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	// Gorilla negotiates no-context-takeover. Small control messages should
	// not pay for a fresh deflate stream; snapshots and row batches benefit.
	r.conn.EnableWriteCompression(len(data) >= 1024)
	err = r.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		r.cancel()
	}
	return err
}
func (r *rpcSocket) chunk(id json.RawMessage, value any) error {
	return r.send(Object{"_tag": "Chunk", "requestId": id, "values": []any{value}})
}
func (r *rpcSocket) exit(id json.RawMessage, value any, err error) {
	exit := Object{"_tag": "Success", "value": value}
	if err != nil {
		exit = Object{"_tag": "Failure", "cause": []any{Object{"_tag": "Die", "defect": Object{"name": "Error", "message": err.Error()}}}}
	}
	_ = r.send(Object{"_tag": "Exit", "requestId": id, "exit": exit})
}
func (r *rpcSocket) request(ctx context.Context, req rpcRequest) {
	s := r.server
	if (req.Method == "orchestration.dispatchCommand" || req.Method == "orchestration.launchThread") && !strings.Contains(" "+r.scopes+" ", " orchestration:operate ") {
		r.exit(req.ID, nil, errors.New("此设备仅获会话读取权限"))
		return
	}
	input := Object{}
	if len(req.Payload) > 0 {
		var obj Object
		if json.Unmarshal(req.Payload, &obj) == nil && obj != nil {
			input = obj
		}
	}
	switch req.Method {
	case "subscribeServerConfig":
		config, err := s.config(ctx)
		if err != nil {
			r.exit(req.ID, nil, err)
			return
		}
		if r.chunk(req.ID, Object{"version": 1, "type": "snapshot", "config": config}) != nil {
			return
		}
		<-ctx.Done()
	case "subscribeServerLifecycle":
		cwd := nonempty(api.WorkspaceProjectPath(s.opts.Workspace), s.opts.DataDir)
		projectName := nonempty(filepath.Base(cwd), "Vantry")
		env := s.descriptor()
		if r.chunk(req.ID, Object{
			"version":  1,
			"sequence": 1,
			"type":     "welcome",
			"payload": Object{
				"environment":     env,
				"cwd":             cwd,
				"projectName":     projectName,
				"bootstrapStatus": "complete",
			},
		}) != nil {
			return
		}
		if r.chunk(req.ID, Object{
			"version":  1,
			"sequence": 2,
			"type":     "ready",
			"payload": Object{
				"at":          utcISO(time.Now()),
				"environment": env,
			},
		}) != nil {
			return
		}
		<-ctx.Done()
	case "subscribeProjectClones":
		if r.chunk(req.ID, []any{}) != nil {
			return
		}
		<-ctx.Done()
	case "subscribeVcsBranch":
		if r.chunk(req.ID, Object{"refName": nil, "isRepo": false}) != nil {
			return
		}
		<-ctx.Done()
	case "subscribeVcsStatus":
		if r.chunk(req.ID, Object{
			"_tag": "snapshot",
			"local": Object{
				"isRepo":                false,
				"hasPrimaryRemote":      false,
				"isDefaultRef":          false,
				"refName":               nil,
				"hasWorkingTreeChanges": false,
				"workingTree":           Object{"files": []any{}, "insertions": 0, "deletions": 0},
			},
			"remote": nil,
		}) != nil {
			return
		}
		<-ctx.Done()
	case "subscribeTerminalMetadata":
		if r.chunk(req.ID, Object{"type": "snapshot", "terminals": []any{}}) != nil {
			return
		}
		<-ctx.Done()
	case "subscribePreviewEvents":
		<-ctx.Done()
	case "subscribeWorktreeSetup":
		if r.chunk(req.ID, nil) != nil {
			return
		}
		<-ctx.Done()
	case "subscribeDiscoveredLocalServers":
		if r.chunk(req.ID, Object{"servers": []any{}, "scannedAt": utcISO(time.Now())}) != nil {
			return
		}
		<-ctx.Done()
	case "scheduledTasks.subscribe":
		if r.chunk(req.ID, Object{"tasks": []any{}}) != nil {
			return
		}
		<-ctx.Done()
	case "orchestration.subscribeShell", "orchestration.subscribeThread":
		r.subscribe(ctx, req, input)
	default:
		value, err := s.call(ctx, req.Method, input)
		if ctx.Err() == nil {
			r.exit(req.ID, value, err)
		}
	}
}
func (s *Server) call(ctx context.Context, method string, input Object) (any, error) {
	switch method {
	case "orchestration.dispatchCommand":
		wire := object(input["command"])
		if len(wire) == 0 {
			wire = input
		}
		return s.dispatch(ctx, wire)
	case "orchestration.launchThread":
		return s.launch(ctx, input)
	case "orchestration.getShellSnapshot":
		return s.shell(ctx)
	case "orchestration.getOlderSettledThreads":
		snap, err := s.service.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return Object{"snapshotSequence": snap.Sequence, "threads": []any{}, "counts": []any{}}, nil
	case "orchestration.getThreadProjection":
		projection, _, err := s.projection(ctx, text(input, "threadId"))
		return projection, err
	case "orchestration.getThreadSnapshot":
		projection, sequence, err := s.projection(ctx, text(input, "threadId"))
		return Object{"snapshotSequence": sequence, "projection": projection}, err
	case "orchestration.getArchivedShellSnapshot":
		shell, err := s.shell(ctx)
		if err != nil {
			return nil, err
		}
		return Object{"schemaVersion": 2, "snapshotSequence": shell["snapshotSequence"], "projects": shell["projects"], "threads": shell["archivedThreads"]}, nil
	case "orchestration.getThreadTitles":
		snapshot, err := s.service.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		titles := Object{}
		ids, _ := input["threadIds"].([]any)
		for _, id := range ids {
			for _, thread := range snapshot.Threads {
				if thread.ID == id {
					titles[thread.ID] = thread.Title
				}
			}
		}
		return titles, nil
	case "server.getConfig", "getServerConfig":
		return s.config(ctx)
	case "server.refreshProviders":
		cfg, err := s.config(ctx)
		if err != nil {
			return nil, err
		}
		return Object{"providers": cfg["providers"]}, nil
	case "server.probe":
		return Object{}, nil
	case "server.reportClientActivity":
		return nil, nil
	case "server.connectionProbe":
		return Object{"serverTime": utcISO(time.Now())}, nil
	case "server.discoverSourceControl":
		return Object{"versionControlSystems": []any{}, "sourceControlProviders": []any{}}, nil
	case "server.getSettings":
		return s.settings(), nil
	case "peerLinks.list":
		return Object{"links": []any{}}, nil
	case "vcs.listRefs":
		return Object{"isAtRepositoryRoot": false, "refs": []any{}, "isRepo": false, "hasPrimaryRemote": false, "nextCursor": nil, "totalCount": 0}, nil
	case "vcs.refreshStatus":
		return Object{
			"isRepo":                false,
			"hasPrimaryRemote":      false,
			"isDefaultRef":          false,
			"refName":               nil,
			"hasWorkingTreeChanges": false,
			"workingTree":           Object{"files": []any{}, "insertions": 0, "deletions": 0},
			"hasUpstream":           false,
			"aheadCount":            0,
			"behindCount":           0,
			"pr":                    nil,
		}, nil
	case "preview.list":
		return Object{"sessions": []any{}, "serverEpoch": nonempty(s.environmentID(), "vantry"), "revision": 0}, nil
	case "preview.reportProfiles":
		return nil, nil
	case "filesystem.getMetadata":
		paths, _ := input["paths"].([]any)
		entries := make([]any, len(paths))
		return Object{"entries": entries}, nil
	case "filesystem.browse":
		return s.browseFilesystem(input)
	case "filesystem.readFile":
		return s.readFilesystemFile(input)
	default:
		log.Printf("t3compat ws rpc unsupported method=%q", method)
		return nil, unsupportedMethod(method)
	}
}

func (s *Server) browseFilesystem(input Object) (Object, error) {
	home, homeErr := os.UserHomeDir()
	raw := nonempty(text(input, "path"), nonempty(text(input, "directory"), text(input, "cwd")))
	if raw == "" {
		if homeErr == nil && home != "" {
			raw = home
		} else if fallback := api.WorkspaceProjectPath(s.opts.Workspace); fallback != "" {
			raw = fallback
		}
	}
	if raw == "" {
		return nil, errors.New("无法确定可浏览的本机目录")
	}
	expanded, err := expandBrowsePath(raw, home)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(expanded)
	if err != nil {
		return nil, fmt.Errorf("无法读取目录 %s：%w", expanded, err)
	}
	showHidden, _ := input["showHidden"].(bool)
	type browseItem struct {
		name  string
		lower string
		entry Object
	}
	items := make([]browseItem, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(expanded, name)
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			if info, statErr := os.Stat(full); statErr == nil && info.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			continue
		}
		_, gitErr := os.Stat(filepath.Join(full, ".git"))
		items = append(items, browseItem{
			name:  name,
			lower: strings.ToLower(name),
			entry: Object{
				"name":        name,
				"path":        full,
				"isDirectory": true,
				"isGitRepo":   gitErr == nil,
			},
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].lower != items[j].lower {
			return items[i].lower < items[j].lower
		}
		return items[i].name < items[j].name
	})
	if len(items) > 500 {
		items = items[:500]
	}
	resultEntries := make([]any, 0, len(items))
	for _, item := range items {
		resultEntries = append(resultEntries, item.entry)
	}
	parent := filepath.Dir(expanded)
	var parentValue any
	if parent != "" && parent != expanded {
		parentValue = parent
	}
	return Object{
		"path":       expanded,
		"parentPath": parentValue,
		"entries":    resultEntries,
	}, nil
}

func expandBrowsePath(raw, home string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsRune(trimmed, 0) || strings.Contains(trimmed, "environment:") {
		return "", errors.New("无效的目录路径")
	}
	if trimmed == "~" {
		if home == "" {
			return "", errors.New("无法解析用户主目录")
		}
		trimmed = home
	} else if strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~\\") {
		if home == "" {
			return "", errors.New("无法解析用户主目录")
		}
		trimmed = filepath.Join(home, trimmed[2:])
	}
	cleaned := filepath.Clean(trimmed)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", err
	}
	return abs, nil
}

const (
	defaultReadFileMaxBytes = 524288
	capReadFileMaxBytes     = 1048576
	maxImageReadBytes       = 8 * 1024 * 1024
	maxPDFReadBytes         = 12 * 1024 * 1024
)

func parsePositiveInt(value any, fallback, limit int) int {
	n := 0
	switch v := value.(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			n = int(parsed)
		}
	}
	if n <= 0 {
		n = fallback
	}
	if limit > 0 && n > limit {
		n = limit
	}
	return n
}

func resolveReadFilePath(rawPath, rawCwd, home string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", errors.New("文件路径不能为空")
	}
	if strings.ContainsRune(trimmed, 0) || strings.Contains(trimmed, "environment:") {
		return "", errors.New("无效的文件路径")
	}
	if trimmed == "~" {
		if home == "" {
			return "", errors.New("无法解析用户主目录")
		}
		trimmed = home
	} else if strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~\\") {
		if home == "" {
			return "", errors.New("无法解析用户主目录")
		}
		trimmed = filepath.Join(home, trimmed[2:])
	} else if !filepath.IsAbs(trimmed) {
		cwd := strings.TrimSpace(rawCwd)
		if cwd != "" {
			expandedCwd, err := expandBrowsePath(cwd, home)
			if err != nil {
				return "", errors.New("无效的文件路径")
			}
			trimmed = filepath.Join(expandedCwd, trimmed)
		}
	}
	cleaned := filepath.Clean(trimmed)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func imageMIMEType(ext string) (string, bool) {
	switch ext {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".bmp":
		return "image/bmp", true
	case ".ico":
		return "image/x-icon", true
	case ".heic":
		return "image/heic", true
	default:
		return "", false
	}
}

func classifyTextFile(name, ext string) (kind, language, mimeType string) {
	lowerName := strings.ToLower(name)
	switch ext {
	case ".md", ".markdown", ".mdx":
		return "markdown", "markdown", "text/markdown; charset=utf-8"
	case ".html", ".htm":
		return "html", "html", "text/html; charset=utf-8"
	case ".svg":
		return "svg", "xml", "image/svg+xml"
	}
	codeLangs := map[string]string{
		".go":         "go",
		".swift":      "swift",
		".ts":         "typescript",
		".tsx":        "tsx",
		".js":         "javascript",
		".jsx":        "jsx",
		".mjs":        "javascript",
		".cjs":        "javascript",
		".py":         "python",
		".rs":         "rust",
		".json":       "json",
		".yaml":       "yaml",
		".yml":        "yaml",
		".toml":       "toml",
		".css":        "css",
		".scss":       "scss",
		".less":       "less",
		".sh":         "bash",
		".bash":       "bash",
		".zsh":        "bash",
		".sql":        "sql",
		".xml":        "xml",
		".c":          "c",
		".h":          "c",
		".cpp":        "cpp",
		".hpp":        "cpp",
		".cc":         "cpp",
		".m":          "objectivec",
		".mm":         "objectivec",
		".java":       "java",
		".kt":         "kotlin",
		".rb":         "ruby",
		".php":        "php",
		".lua":        "lua",
		".zig":        "zig",
		".proto":      "protobuf",
		".graphql":    "graphql",
		".dockerfile": "dockerfile",
	}
	if lang, ok := codeLangs[ext]; ok {
		mime := "text/plain; charset=utf-8"
		if ext == ".json" {
			mime = "application/json; charset=utf-8"
		}
		return "code", lang, mime
	}
	switch lowerName {
	case "dockerfile", "makefile", "rakefile", "gemfile", "podfile", "package.swift":
		return "code", lowerName, "text/plain; charset=utf-8"
	}
	if ext == ".csv" {
		return "text", "csv", "text/csv; charset=utf-8"
	}
	return "text", "text", "text/plain; charset=utf-8"
}

func trimIncompleteUTF8Tail(b []byte) []byte {
	if len(b) == 0 || utf8.Valid(b) {
		return b
	}
	for trim := 1; trim <= 3 && trim <= len(b); trim++ {
		if utf8.Valid(b[:len(b)-trim]) {
			return b[:len(b)-trim]
		}
	}
	return b
}

func (s *Server) readFilesystemFile(input Object) (Object, error) {
	home, _ := os.UserHomeDir()
	cleaned, err := resolveReadFilePath(text(input, "path"), text(input, "cwd"), home)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("文件不存在")
		}
		return nil, fmt.Errorf("无法读取文件：%w", err)
	}
	base := filepath.Base(cleaned)
	if info.IsDir() {
		return Object{
			"path":        cleaned,
			"name":        base,
			"isDirectory": true,
			"kind":        "directory",
			"size":        int64(0),
		}, nil
	}
	size := info.Size()
	ext := strings.ToLower(filepath.Ext(cleaned))
	if mime, isImg := imageMIMEType(ext); isImg {
		if size > maxImageReadBytes {
			return Object{
				"path":        cleaned,
				"name":        base,
				"isDirectory": false,
				"kind":        "image",
				"encoding":    "none",
				"mimeType":    mime,
				"content":     "",
				"size":        size,
				"truncated":   true,
			}, nil
		}
		data, readErr := os.ReadFile(cleaned)
		if readErr != nil {
			return nil, fmt.Errorf("无法读取图片：%w", readErr)
		}
		if mime == "" {
			mime = http.DetectContentType(data)
		}
		return Object{
			"path":        cleaned,
			"name":        base,
			"isDirectory": false,
			"kind":        "image",
			"encoding":    "base64",
			"mimeType":    mime,
			"content":     base64.StdEncoding.EncodeToString(data),
			"size":        size,
			"truncated":   false,
		}, nil
	}
	if ext == ".pdf" {
		if size > maxPDFReadBytes {
			return Object{
				"path":        cleaned,
				"name":        base,
				"isDirectory": false,
				"kind":        "pdf",
				"encoding":    "none",
				"mimeType":    "application/pdf",
				"content":     "",
				"size":        size,
				"truncated":   true,
			}, nil
		}
		data, readErr := os.ReadFile(cleaned)
		if readErr != nil {
			return nil, fmt.Errorf("无法读取 PDF：%w", readErr)
		}
		return Object{
			"path":        cleaned,
			"name":        base,
			"isDirectory": false,
			"kind":        "pdf",
			"encoding":    "base64",
			"mimeType":    "application/pdf",
			"content":     base64.StdEncoding.EncodeToString(data),
			"size":        size,
			"truncated":   false,
		}, nil
	}

	maxBytes := parsePositiveInt(input["maxBytes"], defaultReadFileMaxBytes, capReadFileMaxBytes)
	file, err := os.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("无法打开文件：%w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("无法读取文件：%w", err)
	}
	truncated := len(data) > maxBytes
	if truncated {
		data = trimIncompleteUTF8Tail(data[:maxBytes])
	}
	detectedMIME := http.DetectContentType(data)
	isBinary := bytes.IndexByte(data, 0) >= 0 || (len(data) > 0 && !utf8.Valid(data))
	if isBinary {
		return Object{
			"path":        cleaned,
			"name":        base,
			"isDirectory": false,
			"kind":        "binary",
			"encoding":    "none",
			"mimeType":    detectedMIME,
			"content":     "",
			"size":        size,
			"truncated":   false,
		}, nil
	}
	content := string(data)
	lineCount := 0
	if len(content) > 0 {
		lineCount = strings.Count(content, "\n")
		if !strings.HasSuffix(content, "\n") {
			lineCount++
		}
	}
	kind, language, mimeType := classifyTextFile(base, ext)
	if mimeType == "" {
		mimeType = detectedMIME
	}
	return Object{
		"path":        cleaned,
		"name":        base,
		"isDirectory": false,
		"kind":        kind,
		"language":    language,
		"encoding":    "utf8",
		"mimeType":    mimeType,
		"content":     content,
		"size":        size,
		"lineCount":   lineCount,
		"truncated":   truncated,
	}, nil
}
func (r *rpcSocket) subscribe(ctx context.Context, req rpcRequest, input Object) {
	s := r.server
	threadID := text(input, "threadId")
	snapshot, err := s.service.Snapshot(ctx)
	if err != nil {
		r.exit(req.ID, nil, err)
		return
	}
	// Snapshot then subscribe from its committed cursor: events committed during
	// projection construction are replayed by Core, so reconnects have no gap.
	sub, err := s.service.Subscribe(ctx, snapshot.Sequence)
	if err != nil {
		r.exit(req.ID, nil, err)
		return
	}
	defer sub.Close()
	var previous Object
	// Core coverage and wire ordering are different domains: a single Core
	// event can produce multiple T3 events. Never filter Core with wireSequence.
	coveredCore := snapshot.Sequence
	wireSequence := snapshot.Sequence
	shell := req.Method == "orchestration.subscribeShell"
	acceptCompactTurnItems, _ := input["acceptCompactTurnItems"].(bool)
	publish := func(current api.Snapshot) error {
		if current.Sequence < coveredCore {
			return errors.New("Core 事件游标倒退")
		}
		nextSequence := current.Sequence
		if previous != nil {
			nextSequence = max(nextSequence, wireSequence+1)
		}
		var projection, frame Object
		if shell {
			projection = s.shellSnapshot(current)
			frame = Object{"kind": "snapshot", "snapshot": projection}
		} else {
			var err error
			projection, err = s.project(current, threadID)
			if err != nil {
				return err
			}
			frame = Object{"kind": "snapshot", "snapshotSequence": nextSequence, "projection": projection, "historyCursor": nil, "hasMoreHistory": false, "latestLocalTurnOrdinal": nil}
			if acceptCompactTurnItems {
				if compact, ok := compactThreadProjection(projection); ok {
					frame["projection"] = compact
					frame["turnItemsOmitLocalVisible"] = true
				}
			}
		}
		values := []Object{frame}
		if previous != nil {
			if delta, ok := projectionDelta(previous, projection, wireSequence, current.Sequence, shell); ok {
				// Small threads can cost less as a snapshot. Compare wire sizes,
				// without a content cache or an additional protocol extension.
				fullBytes, _ := json.Marshal(values)
				deltaBytes, _ := json.Marshal(delta)
				if len(deltaBytes) < len(fullBytes) {
					values = delta
				}
			}
		}
		if len(values) > 0 {
			if values[0]["kind"] == "snapshot" {
				if shell {
					// Keep the comparison projection in Core's domain.
					wireSnapshot := make(Object, len(projection))
					for key, value := range projection {
						wireSnapshot[key] = value
					}
					wireSnapshot["snapshotSequence"] = nextSequence
					frame["snapshot"] = wireSnapshot
				}
			} else {
				nextSequence = number(values[len(values)-1]["sequence"])
			}
			if err := r.send(Object{"_tag": "Chunk", "requestId": req.ID, "values": values}); err != nil {
				return err
			}
			wireSequence = nextSequence
		}
		previous, coveredCore = projection, current.Sequence
		return nil
	}
	if err = publish(snapshot); err != nil {
		r.exit(req.ID, nil, err)
		return
	}
	if r.chunk(req.ID, Object{"kind": "synchronized"}) != nil {
		return
	}
	// Coalesce output bursts; only committed Core cursors determine coverage.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	dirty := false
	sequence := snapshot.Sequence
	errorsOut := sub.Errors
	for {
		select {
		case event, ok := <-sub.Events:
			if !ok {
				return
			}
			if event.Sequence > coveredCore && (shell || event.ThreadID == threadID) {
				dirty = true
				sequence = event.Sequence
			}
		case err, ok := <-errorsOut:
			if !ok {
				errorsOut = nil
				continue
			}
			if err != nil {
				r.exit(req.ID, nil, fmt.Errorf("需要重新同步：%w", err))
				return
			}
		case <-ticker.C:
			if dirty {
				current, err := s.service.Snapshot(ctx)
				if err == nil && current.Sequence < sequence {
					err = errors.New("Core 事件游标倒退")
				}
				if err == nil {
					err = publish(current)
				}
				if err != nil {
					r.exit(req.ID, nil, err)
					return
				}
				dirty = false
			}
		case <-ctx.Done():
			return
		}
	}
}
