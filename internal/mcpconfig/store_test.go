package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) Store {
	t.Helper()
	return Store{HomeDir: t.TempDir(), Workspace: t.TempDir()}
}

func put(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectUpsertPreservesUserSecretsAndUnrelatedFields(t *testing.T) {
	s := setup(t)
	user := filepath.Join(s.HomeDir, ".pi/agent/mcp.json")
	put(t, user, `{"mcpServers":{"shared":{"url":"https://example.com/mcp?token=private","headers":{"Authorization":"private"}}}}`)
	file, _ := s.projectFile()
	put(t, file, `{"autoEnableCodemode":false,"mcpServers":{"other":{"command":"rill","args":["mcp"]}}}`)
	if err := s.Upsert("shared", json.RawMessage(`{"enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if strings.Contains(string(b), "private") || !strings.Contains(string(b), "other") || !strings.Contains(string(b), "autoEnableCodemode") {
		t.Fatalf("project override copied credentials or lost unrelated values: %s", b)
	}
	catalog, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if catalog.CodemodeDefault {
		t.Fatal("expected project autoEnableCodemode=false to be reflected")
	}
	for _, server := range catalog.Servers {
		if server.Name == "shared" {
			if server.Scope != ScopeProjectOverride || server.Enabled || server.Endpoint != "https://example.com/mcp" {
				t.Fatalf("bad effective view: %+v", server)
			}
		}
	}
	raw, _ := os.ReadFile(user)
	if !strings.Contains(string(raw), "private") {
		t.Fatal("user configuration was changed")
	}

	// Removing project-override restores inheritance cleanly without touching user file.
	if err := s.RemoveInScope(ScopeProject, "shared"); err != nil {
		t.Fatal(err)
	}
	catalog, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range catalog.Servers {
		if server.Name == "shared" && (server.Scope != ScopeUser || !server.Enabled) {
			t.Fatalf("expected user server restored after removing override, got %+v", server)
		}
	}
}

func TestCodemodeDefaultToolExposureAndWrapperUnwrap(t *testing.T) {
	s := setup(t)
	wrapped := `{"mcpServers":{"gemini-ego":{"command":"node","args":["server.mjs"],"description":"Ask Gemini web","toolExposure":{"fast_*":"direct","debug":"hidden"}}}}`
	if err := s.UpsertInScope(ScopeProject, "gemini-ego", json.RawMessage(wrapped)); err != nil {
		t.Fatalf("upsert wrapped: %v", err)
	}
	file, _ := s.projectFile()
	rawBytes, _ := os.ReadFile(file)
	if !strings.Contains(string(rawBytes), `"exposure": "codemode"`) {
		t.Fatalf("new server must explicitly default to exposure=codemode: %s", rawBytes)
	}
	catalog, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Servers) != 1 || catalog.Servers[0].Exposure != "codemode" || !catalog.CodemodeDefault {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if got := catalog.Servers[0].ToolExposure["fast_*"]; got != "direct" {
		t.Fatalf("toolExposure lost: %+v", catalog.Servers[0].ToolExposure)
	}

	// Invalid toolExposure value must fail.
	if err := s.Upsert("gemini-ego", json.RawMessage(`{"toolExposure":{"a":"bogus"}}`)); err == nil {
		t.Fatal("accepted invalid toolExposure value")
	}
}

func TestUserScopeUpsertRemoveAndEmptyWorkspaceList(t *testing.T) {
	home := t.TempDir()
	s := Store{HomeDir: home}
	if err := s.UpsertInScope(ScopeUser, "magpie", json.RawMessage(`{"url":"https://api.example.com/mcp"}`)); err != nil {
		t.Fatalf("user upsert: %v", err)
	}
	catalog, err := s.List()
	if err != nil {
		t.Fatalf("list without workspace: %v", err)
	}
	if len(catalog.Servers) != 1 || catalog.Servers[0].Scope != ScopeUser || catalog.Servers[0].Exposure != "codemode" {
		t.Fatalf("unexpected empty-workspace catalog: %+v", catalog)
	}
	if err := s.RemoveInScope(ScopeUser, "magpie"); err != nil {
		t.Fatalf("user remove: %v", err)
	}
	catalog, err = s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Servers) != 0 {
		t.Fatalf("expected user server removed, got %+v", catalog.Servers)
	}
}

func TestProjectTrustAncestorLookupAndMalformedGuard(t *testing.T) {
	home := t.TempDir()
	parent := t.TempDir()
	child := filepath.Join(parent, "nested", "repo")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	parentStore := Store{HomeDir: home, Workspace: parent}
	childStore := Store{HomeDir: home, Workspace: child}
	if childStore.IsTrusted() {
		t.Fatal("fresh workspace must not be trusted by default")
	}
	if err := parentStore.SetTrusted(true); err != nil {
		t.Fatal(err)
	}
	if !childStore.IsTrusted() {
		t.Fatal("nearest ancestor trust=true should apply to child")
	}
	if err := childStore.SetTrusted(false); err != nil {
		t.Fatal(err)
	}
	if childStore.IsTrusted() || !parentStore.IsTrusted() {
		t.Fatalf("child explicit untrust mismatch: child=%v parent=%v", childStore.IsTrusted(), parentStore.IsTrusted())
	}

	// Refuse overwriting malformed trust.json.
	put(t, childStore.TrustFile(), "{invalid")
	if err := childStore.SetTrusted(true); err == nil {
		t.Fatal("expected malformed trust.json error")
	}
	if raw, _ := os.ReadFile(childStore.TrustFile()); string(raw) != "{invalid" {
		t.Fatal("malformed trust.json was overwritten")
	}
}

func TestMalformedConfigurationIsNeverOverwritten(t *testing.T) {
	s := setup(t)
	file, _ := s.projectFile()
	put(t, file, `{invalid`)
	if err := s.Upsert("gemini", json.RawMessage(`{"command":"node"}`)); err == nil {
		t.Fatal("expected corrupt config error")
	}
	b, _ := os.ReadFile(file)
	if string(b) != `{invalid` {
		t.Fatal("malformed file was overwritten")
	}
}

func TestRejectInvalidConfigAndNormalizedNameCollision(t *testing.T) {
	s := setup(t)
	for _, raw := range []string{`{"type":"sse","url":"https://example.com/sse"}`, `{"command":"node","url":"https://example.com/mcp"}`, `{"enabled":false}`, `{"command":"node","args":"server.js"}`, `{"url":"not a url"}`} {
		if err := s.Upsert("gemini", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid config %s", raw)
		}
	}
	if err := s.Upsert("gemini-ego", json.RawMessage(`{"command":"node","args":["server.mjs"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert("gemini_ego", json.RawMessage(`{"command":"node"}`)); err == nil {
		t.Fatal("accepted Pi tool namespace collision")
	}
}

func TestPartialServerUpdatePreservesConnection(t *testing.T) {
	s := setup(t)
	if err := s.Upsert("gemini", json.RawMessage(`{"command":"node","args":["server.mjs"],"exposure":"codemode"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert("gemini", json.RawMessage(`{"enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	file, _ := s.projectFile()
	d, _ := read(file)
	m, _ := servers(d)
	c, _ := config(m["gemini"])
	if str(c, "command") != "node" {
		t.Fatal("lost command")
	}
	info, e := s.List()
	if e != nil || len(info.Servers) != 1 || info.Servers[0].Enabled {
		t.Fatalf("bad effective config: %+v %v", info, e)
	}
}
