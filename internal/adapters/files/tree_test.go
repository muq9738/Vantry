package files

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

func writeTestFile(t *testing.T, fullPath string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListDirectorySortingLimitAndSymlinkConfinement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret.txt"), []byte("outside secret"))

	// Create directories, files, .git, internal symlink, and escaping symlinks.
	writeTestFile(t, filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"))
	writeTestFile(t, filepath.Join(root, "z_file.go"), []byte("package main\n"))
	writeTestFile(t, filepath.Join(root, "a_file.txt"), []byte("hello"))
	writeTestFile(t, filepath.Join(root, "zeta_dir", "nested.txt"), []byte("nested"))
	writeTestFile(t, filepath.Join(root, "alpha_dir", "child.go"), []byte("package alpha"))

	if err := os.Symlink(outside, filepath.Join(root, "escape_dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape_file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a_file.txt"), filepath.Join(root, "in_root_link.txt")); err != nil {
		t.Fatal(err)
	}

	entries, err := ListDirectory(ctx, root, "", 100)
	if err != nil {
		t.Fatalf("ListDirectory root failed: %v", err)
	}

	// Expected order: directories first (alpha_dir, zeta_dir), then safe files (a_file.txt, in_root_link.txt, z_file.go).
	// .git, escape_dir, and escape_file.txt must all be absent.
	wantNames := []string{"alpha_dir", "zeta_dir", "a_file.txt", "in_root_link.txt", "z_file.go"}
	if len(entries) != len(wantNames) {
		t.Fatalf("expected %d entries %v, got %+v", len(wantNames), wantNames, entries)
	}
	for i, want := range wantNames {
		if entries[i].Name != want {
			t.Fatalf("entry[%d]: want %q, got %+v", i, want, entries[i])
		}
	}
	if !entries[0].IsDir || entries[0].Size != 0 || !entries[1].IsDir {
		t.Fatalf("directories should come first with size=0: %+v", entries[:2])
	}
	if entries[2].IsDir || entries[2].Size != 5 {
		t.Fatalf("expected file size 5 for a_file.txt, got %+v", entries[2])
	}

	// Limit applies after sorting (limit=3 keeps the 2 dirs + first sorted file).
	limited, err := ListDirectory(ctx, root, ".", 3)
	if err != nil || len(limited) != 3 || limited[2].Name != "a_file.txt" {
		t.Fatalf("unexpected limited entries: %+v, err=%v", limited, err)
	}

	// Subdirectory listing returns root-relative slash paths.
	subEntries, err := ListDirectory(ctx, root, "alpha_dir", 10)
	if err != nil || len(subEntries) != 1 || subEntries[0].Path != "alpha_dir/child.go" {
		t.Fatalf("unexpected subdirectory entries: %+v, err=%v", subEntries, err)
	}

	// Direct traversal, escaping symlink, and .git access must fail.
	for _, forbidden := range []string{"..", "../outside", "/etc", ".git", "escape_dir"} {
		if _, err := ListDirectory(ctx, root, forbidden, 10); err == nil {
			t.Errorf("ListDirectory accepted forbidden path %q", forbidden)
		}
	}
}

func TestReadFilePreviewLineRangesBinaryUTF8And256KiBCap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "leak.txt"), []byte("outside"))

	// 1. Line range + multi-byte UTF-8 across >32KiB chunk boundary.
	lines := make([]string, 0, 1200)
	for i := 1; i <= 1200; i++ {
		lines = append(lines, "第"+strings.Repeat("中", 18)+"行-"+string(rune('A'+(i%26))))
	}
	writeTestFile(t, filepath.Join(root, "src", "unicode.txt"), []byte(strings.Join(lines, "\n")+"\n"))

	preview, err := ReadFilePreview(ctx, root, "src/unicode.txt", 10, 5)
	if err != nil {
		t.Fatalf("ReadFilePreview failed: %v", err)
	}
	if preview.TotalLines != 1200 || preview.StartLine != 10 || preview.EndLine != 14 || !preview.Truncated {
		t.Fatalf("unexpected preview metadata: %+v", preview)
	}
	gotLines := strings.Split(preview.Content, "\n")
	if len(gotLines) != 5 || gotLines[0] != lines[9] || gotLines[4] != lines[13] {
		t.Fatalf("unexpected preview lines: %v", gotLines)
	}

	// Full read of a small file has Truncated=false.
	writeTestFile(t, filepath.Join(root, "small.txt"), []byte("one\ntwo\nthree"))
	small, err := ReadFilePreview(ctx, root, "small.txt", 1, 50)
	if err != nil || small.TotalLines != 3 || small.StartLine != 1 || small.EndLine != 3 || small.Truncated || small.Content != "one\ntwo\nthree" {
		t.Fatalf("unexpected small file preview: %+v, err=%v", small, err)
	}

	// 2. Hard 256 KiB cap on oversized single line with multi-byte runes.
	oversized := strings.Repeat("汉", 120000) + "\nsecond line\n" // ~360 KiB > 256 KiB
	writeTestFile(t, filepath.Join(root, "huge.txt"), []byte(oversized))
	capped, err := ReadFilePreview(ctx, root, "huge.txt", 1, 100)
	if err != nil {
		t.Fatalf("oversized preview failed: %v", err)
	}
	if !capped.Truncated || len(capped.Content) > MaxPreviewBytes || len(capped.Content) < MaxPreviewBytes-4 || !utf8.ValidString(capped.Content) {
		t.Fatalf("256KiB cap violated: truncated=%v len=%d validUTF8=%v", capped.Truncated, len(capped.Content), utf8.ValidString(capped.Content))
	}
	if capped.TotalLines != 2 {
		t.Fatalf("expected TotalLines=2 on oversized file, got %d", capped.TotalLines)
	}

	// 3. Reject NUL binary, invalid UTF-8, directories, FIFOs, and escaping symlinks.
	writeTestFile(t, filepath.Join(root, "binary.bin"), []byte("abc\x00def"))
	writeTestFile(t, filepath.Join(root, "bad_utf8.txt"), []byte{'h', 'i', 0xff, 0xfe, '\n'})
	writeTestFile(t, filepath.Join(root, ".git", "config"), []byte("[core]\n"))
	if err := os.Symlink(filepath.Join(outside, "leak.txt"), filepath.Join(root, "escape_leak.txt")); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(root, "named.pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}

	rejected := []string{
		"binary.bin",
		"bad_utf8.txt",
		"src",
		"named.pipe",
		"escape_leak.txt",
		"../outside/leak.txt",
		".git/config",
	}
	for _, rel := range rejected {
		if _, err := ReadFilePreview(ctx, root, rel, 1, 20); err == nil {
			t.Errorf("ReadFilePreview accepted unsafe/invalid target %q", rel)
		}
	}
}

func TestSearchFilesRespectsGitignoreFallbackWalkAndRankingOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("git-repository-ranking-and-gitignore", func(t *testing.T) {
		repo := t.TempDir()
		outside := t.TempDir()
		writeTestFile(t, filepath.Join(outside, "remote.go"), []byte("outside"))

		gitRun := func(args ...string) {
			t.Helper()
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
			cmd.Env = append(os.Environ(),
				"GIT_CONFIG_NOSYSTEM=1",
				"GIT_CONFIG_GLOBAL="+filepath.Join(repo, "no-global"),
				"GIT_TERMINAL_PROMPT=0",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}

		gitRun("init", "-b", "main")
		gitRun("config", "user.name", "Test")
		gitRun("config", "user.email", "test@example.invalid")
		gitRun("config", "commit.gpgsign", "false")

		writeTestFile(t, filepath.Join(repo, ".gitignore"), []byte("node_modules/\n*.ignored.go\n"))
		// Exact filename match for "remote.go"
		writeTestFile(t, filepath.Join(repo, "internal", "adapters", "git", "remote.go"), []byte("package git\n"))
		// Prefix match for "remote.go"
		writeTestFile(t, filepath.Join(repo, "internal", "adapters", "git", "remote.go.bak"), []byte("backup\n"))
		// Substring match for "remote.go"
		writeTestFile(t, filepath.Join(repo, "internal", "adapters", "my_remote.go"), []byte("package adapters\n"))
		// Fuzzy subsequence match for "remote.go" (r-e-m-o-t-e-_-t-e-s-t-.-g-o)
		writeTestFile(t, filepath.Join(repo, "internal", "adapters", "git", "remote_test.go"), []byte("package git\n"))
		// Ignored files (must NOT appear)
		writeTestFile(t, filepath.Join(repo, "node_modules", "pkg", "remote.go"), []byte("ignored\n"))
		writeTestFile(t, filepath.Join(repo, "secret.ignored.go"), []byte("ignored\n"))
		// Escaping symlink even if untracked (must NOT appear)
		if err := os.Symlink(filepath.Join(outside, "remote.go"), filepath.Join(repo, "escape_remote.go")); err != nil {
			t.Fatal(err)
		}

		gitRun("add", ".gitignore", "internal/adapters/git/remote.go")
		gitRun("commit", "-m", "initial")

		// Note: remote.go.bak, my_remote.go, remote_test.go remain untracked non-ignored;
		// git ls-files -co --exclude-standard -z must discover both tracked and untracked!
		matches, err := SearchFiles(ctx, repo, "@remote.go", 10)
		if err != nil {
			t.Fatalf("SearchFiles failed: %v", err)
		}

		wantOrder := []struct {
			path string
			kind string
		}{
			{path: "internal/adapters/git/remote.go", kind: "exact_name"},
			{path: "internal/adapters/git/remote.go.bak", kind: "prefix_name"},
			{path: "internal/adapters/my_remote.go", kind: "substring"},
			{path: "internal/adapters/git/remote_test.go", kind: "fuzzy"},
		}
		if len(matches) != len(wantOrder) {
			t.Fatalf("expected %d matches, got %+v", len(wantOrder), matches)
		}
		for i, want := range wantOrder {
			if matches[i].Path != want.path || matches[i].MatchKind != want.kind {
				t.Fatalf("match[%d]: want %+v, got %+v", i, want, matches[i])
			}
			if i > 0 && matches[i-1].Score <= matches[i].Score {
				t.Fatalf("scores not strictly descending between %d (%d) and %d (%d)", i-1, matches[i-1].Score, i, matches[i].Score)
			}
		}

		// Multi-word @file query ("git rem") ranks internal/adapters/git/remote.go first.
		multi, err := SearchFiles(ctx, repo, "git rem", 5)
		if err != nil || len(multi) == 0 || multi[0].Path != "internal/adapters/git/remote.go" {
			t.Fatalf("multi-word SearchFiles unexpected result: %+v, err=%v", multi, err)
		}
	})

	t.Run("non-git-directory-bounded-walk", func(t *testing.T) {
		plainDir := t.TempDir()
		outside := t.TempDir()
		writeTestFile(t, filepath.Join(outside, "watcher.go"), []byte("outside"))

		writeTestFile(t, filepath.Join(plainDir, "watcher.go"), []byte("package pr\n"))
		writeTestFile(t, filepath.Join(plainDir, "sub", "watcher_helper.go"), []byte("package sub\n"))
		writeTestFile(t, filepath.Join(plainDir, "sub", "pr_watcher_state.go"), []byte("package sub\n"))
		writeTestFile(t, filepath.Join(plainDir, "deep", "w_a_t_c_h_e_r.txt"), []byte("fuzzy\n"))
		// Cyclic internal symlink and escaping symlink must not loop or leak.
		if err := os.Symlink(plainDir, filepath.Join(plainDir, "sub", "loop")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(plainDir, "outside_link")); err != nil {
			t.Fatal(err)
		}

		matches, err := SearchFiles(ctx, plainDir, "watcher", 10)
		if err != nil {
			t.Fatalf("non-git SearchFiles failed: %v", err)
		}
		if len(matches) != 4 {
			t.Fatalf("expected 4 matches in non-git dir, got %+v", matches)
		}
		// watcher.go (stem exact prefix_name) > watcher_helper.go (prefix_name) > pr_watcher_state.go (substring) > w_a_t_c_h_e_r.txt (fuzzy)
		wantPaths := []string{
			"watcher.go",
			"sub/watcher_helper.go",
			"sub/pr_watcher_state.go",
			"deep/w_a_t_c_h_e_r.txt",
		}
		for i, want := range wantPaths {
			if matches[i].Path != want {
				t.Fatalf("non-git match[%d]: want %q, got %+v", i, want, matches[i])
			}
		}
	})
}
