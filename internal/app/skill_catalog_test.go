package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

// fakeSkillPi answers one get_commands request and records its argv and cwd.
func fakeSkillPi(t *testing.T, dir, commands string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 fixture unavailable")
	}
	path := filepath.Join(dir, "pi")
	script := `#!` + python + `
import json,os,sys
out=os.environ.get("PI_ARGV_FILE")
if out:
    with open(out,"a") as f:
        f.write(os.getcwd()+"\t"+"\n".join(sys.argv[1:])+"\n---\n")
for line in sys.stdin:
    r=json.loads(line)
    if r.get("type")=="get_commands":
        print(json.dumps({"id":r["id"],"type":"response","command":"get_commands","success":True,"data":{"commands":` + commands + `}}),flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func skillFixtureCommands() string {
	return `[
 {"name":"skill:global","description":"user skill","source":"skill","sourceInfo":{"path":"/home/.agents/skills/global/SKILL.md","source":"auto","scope":"user"}},
 {"name":"skill:local","description":"project skill","source":"skill","sourceInfo":{"path":"/ws/.pi/skills/local/SKILL.md","source":"local","scope":"project"}},
 {"name":"plain","description":"prompt template","source":"prompt"}
]`
}

func decodeSkills(t *testing.T, response api.WorkbenchResponse) []api.SkillInfo {
	t.Helper()
	var decoded struct {
		Skills []api.SkillInfo `json:"skills"`
	}
	if err := json.Unmarshal([]byte(response.Output), &decoded); err != nil {
		t.Fatalf("skills.list output is not the contract JSON: %v\n%s", err, response.Output)
	}
	return decoded.Skills
}

func TestSkillsListUsesThreadWorkspaceAndCaches(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	dataDir := filepath.Join(root, "data")
	threadDir := filepath.Join(root, "project")
	otherDir := filepath.Join(root, "other")
	for _, dir := range []string{dataDir, threadDir, otherDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	argv := filepath.Join(root, "argv")
	t.Setenv("PI_ARGV_FILE", argv)
	pi := fakeSkillPi(t, root, skillFixtureCommands())

	service := &workbenchService{
		Service: &baseService{snapshot: api.Snapshot{Threads: []api.Thread{
			{ID: "t1", Workspace: &api.WorkspaceSpec{Kind: "directory", Path: threadDir}},
		}}},
		config: Config{DataDir: dataDir, PiExecutable: pi},
	}

	response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "skills.list", ThreadID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Workspace != threadDir {
		t.Fatalf("workspace attribution lost: %q", response.Workspace)
	}
	skills := decodeSkills(t, response)
	if len(skills) != 2 {
		t.Fatalf("contract output: %+v", skills)
	}
	if skills[0].Name != "global" || skills[0].Command != "skill:global" || skills[0].Source != "user" ||
		skills[0].FilePath != "/home/.agents/skills/global/SKILL.md" {
		t.Fatalf("global skill: %+v", skills[0])
	}
	if skills[1].Name != "local" || skills[1].Source != "project" || skills[1].Command != "skill:local" {
		t.Fatalf("project skill: %+v", skills[1])
	}
	raw, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(raw)), "\t", 2)
	if parts[0] != threadDir {
		t.Fatalf("Pi queried outside the thread workspace: %q", parts[0])
	}

	// A second list within the TTL must not start another Pi process.
	if _, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "skills.list", ThreadID: "t1"}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(argv)
	if err != nil || strings.Count(string(raw), "---") != 1 {
		t.Fatalf("cached listing restarted Pi: %v", raw)
	}

	// An explicit workspace is scoped to that directory, not the home cache.
	response, err = service.Workbench(context.Background(), api.WorkbenchRequest{Action: "skills.list", Workspace: otherDir})
	if err != nil {
		t.Fatal(err)
	}
	if response.Workspace != otherDir {
		t.Fatalf("workspace request resolved elsewhere: %q", response.Workspace)
	}
	raw, _ = os.ReadFile(argv)
	if strings.Count(string(raw), "---") != 2 {
		t.Fatalf("explicit workspace query missing: %v", raw)
	}
	if !strings.Contains(string(raw), otherDir+"\t") {
		t.Fatalf("explicit workspace was not the query cwd: %v", raw)
	}
}

func TestSkillsListRejectsUnknownThread(t *testing.T) {
	root := t.TempDir()
	for _, fallback := range []bool{false, true} {
		service := &workbenchService{
			Service: &baseService{snapshot: api.Snapshot{}},
			config:  Config{DataDir: root},
		}
		request := api.WorkbenchRequest{Action: "skills.list", ThreadID: "missing"}
		if fallback {
			request.Workspace = root
			service.config.Workspace = &api.WorkspaceSpec{Kind: "directory", Path: root}
		}
		_, err := service.Workbench(context.Background(), request)
		if api.CodeOf(err) != api.NotFound {
			t.Fatalf("unknown thread must stay a hard error (fallback=%v): %v", fallback, err)
		}
	}
}

func TestSkillsListBeforeChoosingProject(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(root, "argv")
	t.Setenv("PI_ARGV_FILE", argv)
	service := &workbenchService{
		Service: &baseService{snapshot: api.Snapshot{}},
		config:  Config{DataDir: dataDir, PiExecutable: fakeSkillPi(t, root, skillFixtureCommands())},
	}
	response, err := service.Workbench(context.Background(), api.WorkbenchRequest{Action: "skills.list"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decodeSkills(t, response)) == 0 {
		t.Fatal("projectless draft cannot choose a skill")
	}
	raw, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), dataDir+"\t") {
		t.Fatalf("projectless query inherited the launcher's checkout: %q", raw)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("listing skills prepared a conversation directory: entries=%v err=%v", entries, err)
	}
}
