package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFakeMCPProbePi(t *testing.T, root, exitCode, jsonBody string) string {
	t.Helper()
	script := filepath.Join(root, "pi")
	content := "#!/bin/sh\n" +
		"if [ \"$1\" = \"mcp\" ] && [ \"$2\" = \"list\" ]; then\n" +
		"  printf '%s\\n' " + shellQuote(jsonBody) + "\n" +
		"  exit " + exitCode + "\n" +
		"fi\n" +
		"exit 2\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestProbeMCPParsesExitZeroAndExitOneWithServerError(t *testing.T) {
	t.Run("exit-0-connected", func(t *testing.T) {
		root := t.TempDir()
		body := `{"servers":[{"name":"magpie","scope":"global","enabled":true,"exposure":"codemode","transport":"https://user:secret@example.com/mcp?key=abc#frag","state":"connected","tools":["magpie_search"]}],"errors":[]}`
		bin := writeFakeMCPProbePi(t, root, "0", body)
		out, err := ProbeMCP(context.Background(), bin, root)
		if err != nil {
			t.Fatalf("ProbeMCP: %v", err)
		}
		if len(out.Servers) != 1 || out.Servers[0].State != "connected" || len(out.Servers[0].Tools) != 1 {
			t.Fatalf("unexpected probe output: %+v", out)
		}
		if out.Servers[0].Transport != "https://example.com/mcp" {
			t.Fatalf("transport credentials not redacted: %q", out.Servers[0].Transport)
		}
	})

	t.Run("exit-1-partial-failure-still-decoded", func(t *testing.T) {
		root := t.TempDir()
		body := `{"servers":[{"name":"broken","scope":"project","enabled":true,"exposure":"codemode","transport":"stdio","state":"needs_auth","tools":[],"error":"authentication required"}],"errors":[{"name":"broken","message":"authentication required"}]}`
		bin := writeFakeMCPProbePi(t, root, "1", body)
		out, err := ProbeMCP(context.Background(), bin, root)
		if err != nil {
			t.Fatalf("ProbeMCP should still parse valid JSON on exit 1: %v", err)
		}
		if len(out.Servers) != 1 || out.Servers[0].State != "needs_auth" || out.Servers[0].Error != "authentication required" {
			t.Fatalf("unexpected error-state probe output: %+v", out)
		}
	})
}
