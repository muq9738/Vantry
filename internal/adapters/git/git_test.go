package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergePreservesDirtyWorkAndAbortsConflicts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := command(ctx, root, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Vantry Test")
	run("config", "commit.gpgsign", "false")
	write("base\n")
	run("add", "file.txt")
	run("commit", "-m", "base")
	run("switch", "-c", "feature")
	write("feature\n")
	run("commit", "-am", "feature")
	run("switch", "main")
	write("uncommitted\n")
	if _, err := Merge(ctx, root, "feature"); err == nil {
		t.Fatal("dirty merge accepted")
	}
	content, _ := os.ReadFile(filepath.Join(root, "file.txt"))
	if string(content) != "uncommitted\n" {
		t.Fatal("dirty file changed")
	}
	run("commit", "-am", "main change")
	if _, err := Merge(ctx, root, "feature"); err == nil || !strings.Contains(err.Error(), "恢复") {
		t.Fatalf("conflict not reported/aborted: %v", err)
	}
	if status := run("status", "--porcelain"); status != "" {
		t.Fatalf("conflict cleanup left changes: %s", status)
	}
	content, _ = os.ReadFile(filepath.Join(root, "file.txt"))
	if string(content) != "uncommitted\n" {
		t.Fatal("conflict changed original content")
	}
	run("switch", "-c", "clean-feature")
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "new.txt")
	run("commit", "-m", "new file")
	run("switch", "main")
	if _, err := Merge(ctx, root, "clean-feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal("merge did not bring new file")
	}
	state, err := Inspect(ctx, root)
	if err != nil || state.Branch != "main" || state.Status != "" || state.Conflicts {
		t.Fatalf("Git state: %+v %v", state, err)
	}
}
