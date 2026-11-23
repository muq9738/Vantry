package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteDSNFormatsWindowsAndUnixPaths(t *testing.T) {
	cases := []struct {
		name      string
		slashPath string
		wantPref  string
	}{
		{
			name:      "windows drive path",
			slashPath: "C:/Users/admin/AppData/Roaming/Vantry/core.db",
			wantPref:  "file:///C:/Users/admin/AppData/Roaming/Vantry/core.db?",
		},
		{
			name:      "windows drive path with spaces and special chars",
			slashPath: "C:/Users/admin/Vantry 测试 #1/core.db",
			wantPref:  "file:///C:/Users/admin/Vantry%20%E6%B5%8B%E8%AF%95%20%231/core.db?",
		},
		{
			name:      "unix absolute path",
			slashPath: "/Users/admin/Library/Application Support/Vantry/core.db",
			wantPref:  "file:///Users/admin/Library/Application%20Support/Vantry/core.db?",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqliteDSNFromSlashPath(tc.slashPath)
			if !strings.HasPrefix(got, tc.wantPref) {
				t.Fatalf("sqliteDSNFromSlashPath(%q) = %q, want prefix %q", tc.slashPath, got, tc.wantPref)
			}
			if strings.HasPrefix(got, "file://C:") || strings.HasPrefix(got, "file://c:") {
				t.Fatalf("sqliteDSNFromSlashPath(%q) produced non-empty URI authority: %q", tc.slashPath, got)
			}
			for _, wantPragma := range []string{"busy_timeout", "foreign_keys", "synchronous"} {
				if !strings.Contains(got, wantPragma) {
					t.Fatalf("sqliteDSNFromSlashPath(%q) missing pragma %q: %q", tc.slashPath, wantPragma, got)
				}
			}
		})
	}
}

func TestOpenStoreSupportsSpacesAndUnicodePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Vantry 测试 #1")
	dbPath := filepath.Join(dir, "core.db")
	st, err := openStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("openStore(%q) failed: %v", dbPath, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
}
