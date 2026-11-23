package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRepoFormatsAndRejections(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"egoist/mygo-plugin", "egoist/mygo-plugin"},
		{"  egoist/mygo-plugin.git  ", "egoist/mygo-plugin"},
		{"https://github.com/egoist/mygo-plugin", "egoist/mygo-plugin"},
		{"https://github.com/egoist/mygo-plugin/", "egoist/mygo-plugin"},
		{"https://github.com/egoist/mygo-plugin.git", "egoist/mygo-plugin"},
		{"https://github.com/egoist/mygo-plugin/tree/main", "egoist/mygo-plugin"},
		{"https://github.com/egoist/mygo-plugin/blob/dev/plugin.json", "egoist/mygo-plugin"},
		{"github.com/owner_1/repo.name", "owner_1/repo.name"},
		{"git@github.com:egoist/mygo-plugin.git", "egoist/mygo-plugin"},
		{"git@github.com:egoist/mygo-plugin", "egoist/mygo-plugin"},
		{"ssh://git@github.com/egoist/mygo-plugin.git", "egoist/mygo-plugin"},
	}
	for _, tc := range valid {
		got, err := ParseRepo(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseRepo(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"only-owner",
		"owner/repo/extra",
		"https://gitlab.com/owner/repo",
		"git@gitlab.com:owner/repo.git",
		"https://github.com/only-owner",
		"owner/..",
		"../repo",
	}
	for _, bad := range invalid {
		if got, err := ParseRepo(bad); err == nil {
			t.Fatalf("ParseRepo(%q) unexpectedly succeeded with %q", bad, got)
		}
	}
}

func TestRetiredBundledPluginsDeletedDirectlyWithoutBackup(t *testing.T) {
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	for _, id := range []string{"workspace-insights", "herdr-bridge"} {
		dir := filepath.Join(pluginsDir, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.json"), `{"id":"`+id+`","name":"旧入口","main":"index.ts"}`)
		if err := writeBuiltinMarker(dir, "1.0.0"); err != nil {
			t.Fatal(err)
		}
	}
	staleBackup := filepath.Join(root, "retired-plugins", "old-backup")
	if err := os.MkdirAll(staleBackup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureBuiltins(pluginsDir); err != nil {
		t.Fatal(err)
	}
	list, err := (Host{Directory: pluginsDir}).ListContext(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("wrong plugin remained installed: %+v %v", list, err)
	}
	for _, id := range []string{"workspace-insights", "herdr-bridge"} {
		if _, err := os.Stat(filepath.Join(pluginsDir, id)); !os.IsNotExist(err) {
			t.Fatalf("%s was not deleted directly: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "retired-plugins")); !os.IsNotExist(err) {
		t.Fatalf("retired-plugins backup directory should not exist: %v", err)
	}
}
