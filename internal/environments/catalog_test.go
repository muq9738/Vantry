package environments

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	// A data directory the catalog must create itself, as the desktop does.
	catalog, err := New(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func validEntry(name, host string) EnvironmentEntry {
	return EnvironmentEntry{Name: name, Host: host, DefaultWorkspace: "/srv/work"}
}

func TestUpsertLoadAndPermissions(t *testing.T) {
	catalog := newTestCatalog(t)
	if entries, err := catalog.Load(); err != nil || len(entries) != 0 {
		t.Fatalf("missing catalog should load empty: %v %v", entries, err)
	}
	first, err := catalog.Upsert(validEntry("MacBook", "me@macbook"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "macbook" || first.CreatedAt.IsZero() {
		t.Fatalf("upsert = %+v", first)
	}
	second, err := catalog.Upsert(EnvironmentEntry{Name: "GPU Server", Host: "gpu", SocketPath: "/run/vantry/core.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != "gpu-server" {
		t.Fatalf("derived id = %q", second.ID)
	}
	info, err := os.Stat(catalog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("catalog permissions = %o, want 600", perm)
	}
	if dirInfo, err := os.Stat(filepath.Dir(catalog.Path())); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("data dir = %v %v", dirInfo, err)
	}
	leftovers, err := os.ReadDir(filepath.Dir(catalog.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 1 || leftovers[0].Name() != FileName {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
	reopened, err := New(filepath.Dir(catalog.Path()))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.Load()
	if err != nil || len(entries) != 2 || entries[1].SocketPath != "/run/vantry/core.sock" {
		t.Fatalf("reloaded = %+v %v", entries, err)
	}
	if entries[0].Socket() != DefaultRemoteSocket {
		t.Fatalf("default socket = %q", entries[0].Socket())
	}
}

func TestUpsertReplacesInPlaceAndKeepsHistory(t *testing.T) {
	catalog := newTestCatalog(t)
	created, err := catalog.Upsert(validEntry("MacBook", "me@macbook"))
	if err != nil {
		t.Fatal(err)
	}
	connected, err := catalog.TouchConnected(created.ID, time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := catalog.Upsert(EnvironmentEntry{ID: created.ID, Name: "MacBook Pro", Host: "me@macbook", SocketPath: "/tmp/core.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "MacBook Pro" || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("update lost creation time: %+v", updated)
	}
	if !updated.LastConnectedAt.Equal(connected.LastConnectedAt) {
		t.Fatalf("update lost last connection: %+v", updated)
	}
	entries, err := catalog.Load()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v %v", entries, err)
	}
	if entries[0].SocketPath != "/tmp/core.sock" || entries[0].Name != "MacBook Pro" {
		t.Fatalf("entry = %+v", entries[0])
	}
	if _, err := catalog.Upsert(validEntry("Second", "second")); err != nil {
		t.Fatal(err)
	}
	entries, _ = catalog.Load()
	if len(entries) != 2 || entries[0].ID != created.ID {
		t.Fatalf("a replacement must keep its position: %+v", entries)
	}
}

func TestUpsertDerivesUniqueIDsAndSavesDefaultsImplicitly(t *testing.T) {
	catalog := newTestCatalog(t)
	for i := 0; i < 2; i++ {
		if _, err := catalog.Upsert(validEntry("Work", "work")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := catalog.Load()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v %v", entries, err)
	}
	if entries[0].ID != "work" || entries[1].ID != "work-2" {
		t.Fatalf("ids = %q %q", entries[0].ID, entries[1].ID)
	}
	explicit, err := catalog.Upsert(EnvironmentEntry{ID: "gpu", Name: "GPU", Host: "gpu", SocketPath: DefaultRemoteSocket})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.SocketPath != "" {
		t.Fatalf("the documented default socket should stay implicit: %q", explicit.SocketPath)
	}
	if explicit.Socket() != DefaultRemoteSocket {
		t.Fatalf("socket = %q", explicit.Socket())
	}
}

func TestRemoveAndTouchConnected(t *testing.T) {
	catalog := newTestCatalog(t)
	entry, err := catalog.Upsert(validEntry("GPU Server", "gpu"))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Remove("missing"); err == nil {
		t.Fatal("removing an unknown environment should fail")
	}
	if _, err := catalog.TouchConnected("missing", time.Time{}); err == nil {
		t.Fatal("touching an unknown environment should fail")
	}
	touched, err := catalog.TouchConnected(entry.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if touched.LastConnectedAt.IsZero() || time.Since(touched.LastConnectedAt) > time.Minute {
		t.Fatalf("touch = %+v", touched)
	}
	if err := catalog.Remove(entry.ID); err != nil {
		t.Fatal(err)
	}
	if entries, err := catalog.Load(); err != nil || len(entries) != 0 {
		t.Fatalf("entries after remove = %+v %v", entries, err)
	}
}

func TestValidateEntryKeepsTheSSHBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		entry EnvironmentEntry
		want  string
	}{
		{"option host", EnvironmentEntry{Name: "x", Host: "-oProxyCommand=touch /tmp/pwned"}, "SSH"},
		{"newline host", EnvironmentEntry{Name: "x", Host: "host\nrm -rf /"}, "SSH"},
		{"host with space", EnvironmentEntry{Name: "x", Host: "host --foo"}, "SSH"},
		{"empty host", EnvironmentEntry{Name: "x"}, "SSH"},
		{"relative socket", EnvironmentEntry{Name: "x", Host: "h", SocketPath: "run/core.sock"}, "绝对路径"},
		{"socket with colon", EnvironmentEntry{Name: "x", Host: "h", SocketPath: "/run/core.sock:1"}, "冒号"},
		{"bare tilde", EnvironmentEntry{Name: "x", Host: "h", SocketPath: "~"}, "socket"},
		{"other user tilde", EnvironmentEntry{Name: "x", Host: "h", SocketPath: "~root/core.sock"}, "socket"},
		{"empty name", EnvironmentEntry{Host: "h"}, "名称"},
		{"newline name", EnvironmentEntry{Name: "a\nb", Host: "h"}, "控制字符"},
		{"padded name", EnvironmentEntry{Name: " a ", Host: "h"}, "空白"},
		{"bad id", EnvironmentEntry{ID: "a/b", Name: "x", Host: "h"}, "ID"},
		{"relative workspace", EnvironmentEntry{Name: "x", Host: "h", DefaultWorkspace: "work"}, "绝对路径"},
		{"workspace newline", EnvironmentEntry{Name: "x", Host: "h", DefaultWorkspace: "/work\nx"}, "控制字符"},
	}
	for _, tc := range cases {
		err := ValidateEntry(tc.entry)
		if err == nil {
			t.Errorf("%s: accepted %+v", tc.name, tc.entry)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
	good := []EnvironmentEntry{
		{ID: "mac", Name: "MacBook", Host: "me@macbook", DefaultWorkspace: "/srv/app"},
		{ID: "gpu", Name: "GPU Server", Host: "gpu", SocketPath: "/run/vantry/core.sock"},
		{ID: "home", Name: "家里", Host: "home", SocketPath: DefaultRemoteSocket, DefaultWorkspace: "~/code/app"},
		{ID: "alias", Name: "Alias", Host: "prod.example.com"},
	}
	for _, entry := range good {
		if err := ValidateEntry(entry); err != nil {
			t.Errorf("rejected %+v: %v", entry, err)
		}
	}
}

func TestSaveRejectsDuplicateIDsAndInvalidEntries(t *testing.T) {
	catalog := newTestCatalog(t)
	if err := catalog.Save([]EnvironmentEntry{validEntry("a", "a"), validEntry("b", "b")}); err == nil {
		t.Fatal("entries without ids should not save")
	}
	if err := catalog.Save([]EnvironmentEntry{
		{ID: "same", Name: "a", Host: "a"},
		{ID: "same", Name: "b", Host: "b"},
	}); err == nil {
		t.Fatal("duplicate ids should not save")
	}
	if err := catalog.Save([]EnvironmentEntry{{ID: "bad", Name: "x", Host: "-oProxyCommand=x"}}); err == nil {
		t.Fatal("an invalid entry should not save")
	}
	if _, err := os.Stat(catalog.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected save must not touch the file: %v", err)
	}
	if err := catalog.Save([]EnvironmentEntry{{ID: "ok", Name: "OK", Host: "ok"}}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportsBrokenCatalogs(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{"version":1,"environments":[`,
		"future version": `{"version":99,"environments":[]}`,
		"no id":          `{"version":1,"environments":[{"name":"x","host":"h"}]}`,
		"duplicate id":   `{"version":1,"environments":[{"id":"a","name":"x","host":"h"},{"id":"a","name":"y","host":"i"}]}`,
		"invalid host":   `{"version":1,"environments":[{"id":"a","name":"x","host":"-oProxyCommand=x"}]}`,
	}
	for name, body := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		catalog, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Load(); err == nil {
			t.Errorf("%s: loaded without an error", name)
		}
	}
}

func TestProbeAllReportsEveryHostInOrder(t *testing.T) {
	catalog := newTestCatalog(t)
	for _, entry := range []EnvironmentEntry{
		{ID: "a", Name: "Alpha", Host: "alpha"},
		{ID: "b", Name: "Beta", Host: "beta"},
		{ID: "c", Name: "Gamma", Host: "gamma"},
		{ID: "d", Name: "Delta", Host: "delta"},
		{ID: "e", Name: "Epsilon", Host: "epsilon"},
	} {
		if _, err := catalog.Upsert(entry); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	peak, running := 0, 0
	statuses := catalog.ProbeAll(context.Background(), func(ctx context.Context, entry EnvironmentEntry) ProbeResult {
		mu.Lock()
		running++
		peak = max(peak, running)
		seen[entry.Host]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		if entry.Host == "beta" {
			return ProbeResult{Reachable: false, LatencyMs: 42, Detail: "连接超时"}
		}
		return ProbeResult{Reachable: true, LatencyMs: 7, Detail: "远端 Core socket 存在"}
	})
	if len(statuses) != 5 {
		t.Fatalf("statuses = %d", len(statuses))
	}
	var order []string
	for _, status := range statuses {
		order = append(order, status.Environment.ID)
		if status.CheckedAt.IsZero() {
			t.Errorf("%s has no check time", status.Environment.Name)
		}
	}
	if strings.Join(order, ",") != "a,b,c,d,e" {
		t.Fatalf("order = %v", order)
	}
	if statuses[1].Reachable || statuses[1].Detail != "连接超时" || statuses[1].LatencyMs != 42 {
		t.Fatalf("beta = %+v", statuses[1])
	}
	if !statuses[0].Reachable || statuses[0].LatencyMs != 7 {
		t.Fatalf("alpha = %+v", statuses[0])
	}
	if peak > probeWorkers {
		t.Fatalf("ran %d probes at once, limit is %d", peak, probeWorkers)
	}
	for host, count := range seen {
		if count != 1 {
			t.Errorf("%s probed %d times", host, count)
		}
	}
	if statuses := catalog.ProbeAll(context.Background(), nil); statuses != nil {
		_ = statuses // a nil probe falls back to ssh; nothing to assert without a host
	}
}

func TestProbeAllSurvivesACancelledContextAndAPanickingProbe(t *testing.T) {
	catalog := newTestCatalog(t)
	for _, entry := range []EnvironmentEntry{{ID: "a", Name: "A", Host: "a"}, {ID: "b", Name: "B", Host: "b"}} {
		if _, err := catalog.Upsert(entry); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	statuses := catalog.ProbeAll(ctx, func(ctx context.Context, entry EnvironmentEntry) ProbeResult {
		if entry.ID == "b" {
			panic("probe blew up")
		}
		return ProbeResult{Detail: "已取消"}
	})
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d", len(statuses))
	}
	if statuses[0].Reachable || statuses[0].Detail != "已取消" {
		t.Fatalf("a = %+v", statuses[0])
	}
	if statuses[1].Reachable || !strings.Contains(statuses[1].Detail, "probe blew up") {
		t.Fatalf("a panicking probe must become a status, got %+v", statuses[1])
	}
	if statuses[1].CheckedAt.IsZero() {
		t.Fatal("a failed probe still needs a check time")
	}
	if statuses := catalog.ProbeAll(nil, func(context.Context, EnvironmentEntry) ProbeResult { return ProbeResult{} }); len(statuses) != 2 {
		t.Fatalf("a nil context should still probe: %d", len(statuses))
	}
}

func TestProbeCommandQuotesTheSocket(t *testing.T) {
	cases := map[string]string{
		DefaultRemoteSocket:          "test -S ~/.vantry/core.sock",
		"/run/vantry/core.sock":      "test -S /run/vantry/core.sock",
		"/tmp/core socket.sock":      "test -S '/tmp/core socket.sock'",
		"/tmp/it's.sock":             `test -S '/tmp/it'\''s.sock'`,
		"/tmp/$(rm -rf x)/sock.sock": "test -S '/tmp/$(rm -rf x)/sock.sock'",
	}
	for socket, want := range cases {
		if got := probeCommand(socket); got != want {
			t.Errorf("probeCommand(%q) = %q, want %q", socket, got, want)
		}
	}
	args := probeArgs(EnvironmentEntry{Host: "me@macbook", SocketPath: "/run/core.sock"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-T", "BatchMode=yes", "StrictHostKeyChecking=yes", "-- me@macbook test -S /run/core.sock"} {
		if !strings.Contains(joined, want) {
			t.Errorf("probe args %q missing %q", joined, want)
		}
	}
}

func TestResolveSocketExpandsTheRemoteHome(t *testing.T) {
	entry := EnvironmentEntry{Name: "x", Host: "h"}
	if socket, err := entry.ResolveSocket("/home/me"); err != nil || socket != "/home/me/.vantry/core.sock" {
		t.Fatalf("resolve = %q %v", socket, err)
	}
	if _, err := entry.ResolveSocket(""); err == nil {
		t.Fatal("a ~ socket needs a remote home")
	}
	absolute := EnvironmentEntry{Name: "x", Host: "h", SocketPath: "/run/core.sock"}
	if socket, err := absolute.ResolveSocket(""); err != nil || socket != "/run/core.sock" {
		t.Fatalf("absolute resolve = %q %v", socket, err)
	}
}

func TestCatalogSerializesConcurrentWriters(t *testing.T) {
	catalog := newTestCatalog(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := catalog.Upsert(EnvironmentEntry{Name: "Host", Host: "host-" + string(rune('a'+i))}); err != nil {
				t.Errorf("upsert %d: %v", i, err)
			}
			if _, err := catalog.Load(); err != nil {
				t.Errorf("load %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	entries, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 12 {
		t.Fatalf("lost upserts: %d entries", len(entries))
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.ID == "" || seen[entry.ID] {
			t.Fatalf("bad id after concurrent writes: %+v", entry)
		}
		seen[entry.ID] = true
	}
	raw, err := os.ReadFile(catalog.Path())
	if err != nil {
		t.Fatal(err)
	}
	var doc catalogFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("concurrent writes produced an unreadable document: %v", err)
	}
	if doc.Version != fileVersion || len(doc.Environments) != 12 {
		t.Fatalf("document = %+v", doc)
	}
}

func TestSortByNameIsStable(t *testing.T) {
	entries := []EnvironmentEntry{
		{ID: "b", Name: "beta", CreatedAt: time.Unix(20, 0)},
		{ID: "a", Name: "Alpha", CreatedAt: time.Unix(10, 0)},
		{ID: "c", Name: "beta", CreatedAt: time.Unix(5, 0)},
	}
	SortByName(entries)
	var order []string
	for _, entry := range entries {
		order = append(order, entry.ID)
	}
	if strings.Join(order, ",") != "a,c,b" {
		t.Fatalf("order = %v", order)
	}
}

func TestPersistedDocumentOmitsAnUnusedLastConnection(t *testing.T) {
	catalog := newTestCatalog(t)
	if _, err := catalog.Upsert(validEntry("MacBook", "me@macbook")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(catalog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lastConnectedAt") {
		t.Fatalf("a never-connected entry should not record a connection: %s", raw)
	}
	if !strings.Contains(string(raw), "\"version\": 1") || !strings.Contains(string(raw), "\"host\": \"me@macbook\"") {
		t.Fatalf("document = %s", raw)
	}
	entry, err := catalog.Upsert(validEntry("MacBook", "me@macbook"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.TouchConnected(entry.ID, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(catalog.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"lastConnectedAt": "2026-01-02T03:04:05Z"`) {
		t.Fatalf("document = %s", raw)
	}
}

func TestValidateAcceptsOnlyTheDocumentedTildeForm(t *testing.T) {
	if err := ValidateEntry(EnvironmentEntry{Name: "x", Host: "h", SocketPath: "~/.vantry/other.sock"}); err != nil {
		t.Fatalf("a ~/ socket should pass: %v", err)
	}
	for _, socket := range []string{"~", "~home/x.sock", "relative/x.sock", "/run/a:b.sock", "~/"} {
		if err := ValidateEntry(EnvironmentEntry{Name: "x", Host: "h", SocketPath: socket}); err == nil {
			t.Errorf("accepted socket %q", socket)
		}
	}
}
