package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakePi writes a python fixture that answers one get_commands request with the
// supplied command entries and records its argv for policy assertions.
func fakePi(t *testing.T, commands string, body string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	path := filepath.Join(t.TempDir(), "pi")
	script := `#!` + python + `
import json,os,sys
argv=os.environ.get("PI_ARGV_FILE")
if argv:
    with open(argv,"w") as f:
        f.write("\n".join(sys.argv[1:]))
` + body + `
for line in sys.stdin:
    r=json.loads(line)
    if r.get("type")=="get_commands":
        print(json.dumps({"type":"notification"}),flush=True)
        print(json.dumps({"id":r["id"],"type":"response","command":"get_commands","success":True,"data":{"commands":` + commands + `}}),flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestSkillsQueriesPiCommandsAndProjectsSkillMetadata(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("PI_ARGV_FILE", argv)
	commands := `[
 {"name":"review","description":"Not a skill","source":"prompt","sourceInfo":{"path":"/p/review.md","source":"auto","scope":"project"}},
 {"name":"skill:docx","description":"Word documents","source":"skill","sourceInfo":{"path":"/home/.agents/skills/docx/SKILL.md","source":"auto","scope":"user"}},
 {"name":"skill:deploy","description":"Deploys","source":"skill","sourceInfo":{"path":"/repo/.agents/skills/deploy/SKILL.md","source":"local","scope":"project"}},
 {"name":"skill:alpha","description":"No info","source":"skill"},
 {"name":"skill:docx","description":"Duplicate shadowed name","source":"skill","sourceInfo":{"path":"/elsewhere/SKILL.md","source":"auto","scope":"project"}},
 {"name":"skill:beta","description":"b","source":"skill","sourceInfo":{"path":"/b/SKILL.md","source":"auto","scope":"user"}},
 {"name":"ext-cmd","source":"extension"}
]`
	pi := fakePi(t, commands, "")
	skills, err := Skills(context.Background(), pi, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 4 {
		t.Fatalf("skills: %+v", skills)
	}
	if skills[0].Name != "alpha" || skills[0].Command != "skill:alpha" || skills[0].Source != "skill" || skills[0].FilePath != "" {
		t.Fatalf("skill fallback metadata wrong: %+v", skills[0])
	}
	if skills[1].Name != "beta" || skills[3].Name != "docx" {
		t.Fatalf("skills not sorted or deduplicated: %+v", skills)
	}
	docx := skills[3]
	if docx.Description != "Word documents" || docx.FilePath != "/home/.agents/skills/docx/SKILL.md" || docx.Source != "user" || docx.Command != "skill:docx" {
		t.Fatalf("docx metadata: %+v", docx)
	}
	deploy := skills[2]
	if deploy.Name != "deploy" || deploy.Source != "project" || deploy.FilePath != "/repo/.agents/skills/deploy/SKILL.md" {
		t.Fatalf("project skill: %+v", deploy)
	}
	args := readArgv(t, argv)
	for _, flag := range []string{"--mode", "rpc", "--no-session", "--no-tools", "--no-prompt-templates"} {
		found := false
		for _, arg := range args {
			found = found || arg == flag
		}
		if !found {
			t.Fatalf("missing %s in %v", flag, args)
		}
	}
}

func TestSkillsMirrorsWorkerExtensionPolicy(t *testing.T) {
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("PI_ARGV_FILE", argv)
	pi := fakePi(t, `[]`, "")

	// An opted-in Core must let Pi load the same user extensions and MCP a
	// worker would, otherwise their commands disappear from the skill menu.
	t.Setenv("VANTRY_PI_USER_EXTENSIONS", "1")
	t.Setenv("VANTRY_PI_MCP_UNSET_MARKER", "")
	t.Setenv("VANTRY_PI_USER_MCP", "true")
	if _, err := Skills(context.Background(), pi, ""); err != nil {
		t.Fatal(err)
	}
	for _, arg := range readArgv(t, argv) {
		if arg == "--no-extensions" || arg == "--no-mcp" {
			t.Fatalf("opted-in policy still disabled resources: %v", arg)
		}
	}

	t.Setenv("VANTRY_PI_USER_EXTENSIONS", "0")
	t.Setenv("VANTRY_PI_USER_MCP", "off")
	if _, err := Skills(context.Background(), pi, ""); err != nil {
		t.Fatal(err)
	}
	args := readArgv(t, argv)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--no-extensions") || !strings.Contains(joined, "--no-mcp") {
		t.Fatalf("default policy lost worker isolation flags: %v", args)
	}
}

func TestSkillsReportsRejectedQuery(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	pi := filepath.Join(t.TempDir(), "pi")
	script := `#!` + python + `
import json,sys
for line in sys.stdin:
    r=json.loads(line)
    print(json.dumps({"id":r["id"],"type":"response","command":r["type"],"success":False,"error":"unknown command"}),flush=True)
`
	if err := os.WriteFile(pi, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = Skills(context.Background(), pi, "")
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("rejected get_commands must surface Pi's error: %v", err)
	}
}

func TestSkillsCancelledQueryReapsProcess(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	pi := filepath.Join(t.TempDir(), "pi")
	// This fixture never answers; cancellation must still reap the child.
	script := `#!` + python + `
import os,sys,time
with open(` + strconv.Quote(pidFile) + `,"w") as f:
    f.write(str(os.getpid()))
time.sleep(60)
`
	if err := os.WriteFile(pi, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Skills(ctx, pi, "")
		done <- err
	}()
	// Wait until the child is definitely running so cancellation exercises the
	// kill-and-reap path instead of winning the race before spawn.
	pid := 0
	startDeadline := time.Now().Add(10 * time.Second)
	for pid == 0 && time.Now().Before(startDeadline) {
		if raw, readErr := os.ReadFile(pidFile); readErr == nil {
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); scanErr != nil {
				pid = 0
			}
		}
		if pid == 0 {
			select {
			case err := <-done:
				t.Fatalf("query ended before the fixture started: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	if pid == 0 {
		t.Fatal("fixture never started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled query reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled query did not return")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				break
			}
			t.Fatalf("probe child pid %d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled Pi process %d still running", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestInstalledPiSkillsOptIn(t *testing.T) {
	if os.Getenv("VANTRY_SKILLS_TEST") != "1" {
		t.Skip("set VANTRY_SKILLS_TEST=1 to query installed Pi without prompting a model")
	}
	skills, err := Skills(context.Background(), "pi", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) == 0 {
		t.Fatal("installed Pi returned no skills")
	}
	var docx bool
	for _, s := range skills {
		if s.Name == "" || s.Command != "skill:"+s.Name {
			t.Fatalf("skill contract violated: %+v", s)
		}
		docx = docx || s.Name == "docx"
	}
	t.Logf("Pi skills=%d docx=%v first=%s command=%s", len(skills), docx, skills[0].Name, skills[0].Command)
}
