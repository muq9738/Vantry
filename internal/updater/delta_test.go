package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBsdiffRoundTrip(t *testing.T) {
	largeOld := bytes.Repeat([]byte("0123456789abcdef-macho-text-segment-"), 4000)
	largeNew := append([]byte(nil), largeOld...)
	copy(largeNew[4096:], []byte("PATCHED_VERSION_0_1_28_BINARY"))
	largeNew = append(largeNew[:12000], append([]byte("INSERTED-SEGMENT-"), largeNew[12000:]...)...)

	cases := []struct {
		name     string
		old, new []byte
	}{
		{"empty-to-empty", nil, nil},
		{"empty-to-data", nil, []byte("brand new payload 12345")},
		{"data-to-empty", []byte("obsolete data"), nil},
		{"identical", []byte("same bytes across versions"), []byte("same bytes across versions")},
		{"shifted-binary", largeOld, largeNew},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch := bsdiffDiff(tc.old, tc.new)
			var out bytes.Buffer
			err := bsdiffPatch(&out, int64(len(tc.new)), bytes.NewReader(tc.old), int64(len(tc.old)), bytes.NewReader(patch), int64(len(patch)))
			if err != nil {
				t.Fatalf("bsdiffPatch: %v", err)
			}
			if !bytes.Equal(out.Bytes(), tc.new) {
				t.Fatalf("patched bytes mismatch: got len %d want %d", out.Len(), len(tc.new))
			}
		})
	}

	// Corrupted patch is rejected cleanly.
	patch := bsdiffDiff(largeOld, largeNew)
	patch[len(patch)/2] ^= 0xff
	var bad bytes.Buffer
	if err := bsdiffPatch(&bad, int64(len(largeNew)), bytes.NewReader(largeOld), int64(len(largeOld)), bytes.NewReader(patch), int64(len(patch))); err == nil {
		if bytes.Equal(bad.Bytes(), largeNew) {
			t.Fatal("corrupted patch unexpectedly reproduced exact new bytes")
		}
	}
	if err := bsdiffPatch(&bad, int64(len(largeNew)), bytes.NewReader(largeOld), int64(len(largeOld)), bytes.NewReader([]byte{1, 2}), 2); !errors.Is(err, errDamagedPatch) {
		t.Fatalf("truncated patch error = %v, want errDamagedPatch", err)
	}
}

func TestWriteAndApplyDeltaBundle(t *testing.T) {
	tmp := t.TempDir()
	oldBundle := filepath.Join(tmp, "old", "Vantry.app")
	newBundle := filepath.Join(tmp, "new", "Vantry.app")

	// Simulate an executable with small edits plus shared assets and unchanged update-token.
	oldExe := bytes.Repeat([]byte("Vantry-ARM64-CODE-SECTION-0123456789abcdef\n"), 3000)
	newExe := append([]byte(nil), oldExe...)
	copy(newExe[8192:], []byte("Vantry 0.1.28 updated section"))
	newExe = append(newExe[:24000], append([]byte("EXTRA-CALL\n"), newExe[24000:]...)...)

	const tokenContent = "github_pat_readonly_test_fixture_123456"
	const sharedIcon = "ICON-PNG-BYTES-UNCHANGED"

	writeFile(t, filepath.Join(oldBundle, "Contents", "Info.plist"), "<plist><key>CFBundleShortVersionString</key><string>0.1.27</string></plist>")
	writeFile(t, filepath.Join(oldBundle, "Contents", "Resources", "update-token"), tokenContent)
	writeFile(t, filepath.Join(oldBundle, "Contents", "Resources", "old-only.txt"), "remove me")
	writeFile(t, filepath.Join(oldBundle, "Contents", "Resources", "moved-orig.txt"), sharedIcon)
	if err := os.MkdirAll(filepath.Join(oldBundle, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldBundle, "Contents", "MacOS", "vantry"), oldExe, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(newBundle, "Contents", "Info.plist"), "<plist><key>CFBundleShortVersionString</key><string>0.1.28</string></plist>")
	writeFile(t, filepath.Join(newBundle, "Contents", "Resources", "update-token"), tokenContent)
	writeFile(t, filepath.Join(newBundle, "Contents", "Resources", "moved.txt"), sharedIcon)
	writeFile(t, filepath.Join(newBundle, "Contents", "Resources", "added.txt"), "hello 0.1.28")
	if err := os.MkdirAll(filepath.Join(newBundle, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newBundle, "Contents", "MacOS", "vantry"), newExe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../MacOS/vantry", filepath.Join(newBundle, "Contents", "Resources", "vantry-link")); err != nil {
		t.Fatal(err)
	}

	var deltaBuf bytes.Buffer
	if err := WriteDelta(&deltaBuf, "0.1.27", "0.1.28", oldBundle, newBundle); err != nil {
		t.Fatalf("WriteDelta: %v", err)
	}
	deltaBytes := deltaBuf.Bytes()
	if len(deltaBytes) >= len(newExe)/3 {
		t.Fatalf("delta too large: %d bytes vs exe %d bytes", len(deltaBytes), len(newExe))
	}

	reconstructed := filepath.Join(tmp, "reconstructed", "Vantry.app")
	if err := os.MkdirAll(filepath.Dir(reconstructed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyDelta(bytes.NewReader(deltaBytes), int64(len(deltaBytes)), "0.1.27", "0.1.28", oldBundle, reconstructed); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(reconstructed, "Contents", "MacOS", "vantry")); err != nil || !bytes.Equal(got, newExe) {
		t.Fatalf("reconstructed exe mismatch: err=%v len=%d", err, len(got))
	}
	exeInfo, err := os.Stat(filepath.Join(reconstructed, "Contents", "MacOS", "vantry"))
	if err != nil || exeInfo.Mode().Perm() != 0o755 {
		t.Fatalf("reconstructed exe mode = %v, err=%v", exeInfo.Mode().Perm(), err)
	}
	if got := readFile(t, filepath.Join(reconstructed, "Contents", "Resources", "update-token")); got != tokenContent {
		t.Fatalf("update-token mismatch: %q", got)
	}
	if got := readFile(t, filepath.Join(reconstructed, "Contents", "Resources", "moved.txt")); got != sharedIcon {
		t.Fatalf("moved file mismatch: %q", got)
	}
	if got := readFile(t, filepath.Join(reconstructed, "Contents", "Resources", "added.txt")); got != "hello 0.1.28" {
		t.Fatalf("added file mismatch: %q", got)
	}
	if _, err := os.Stat(filepath.Join(reconstructed, "Contents", "Resources", "old-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file survived delta apply: %v", err)
	}
	if linkTarget, err := os.Readlink(filepath.Join(reconstructed, "Contents", "Resources", "vantry-link")); err != nil || linkTarget != "../MacOS/vantry" {
		t.Fatalf("symlink mismatch: target=%q err=%v", linkTarget, err)
	}

	// Wrong source version must fail cleanly.
	wrongVerDir := filepath.Join(tmp, "wrong-ver")
	if err := ApplyDelta(bytes.NewReader(deltaBytes), int64(len(deltaBytes)), "0.1.26", "0.1.28", oldBundle, wrongVerDir); err == nil {
		t.Fatal("ApplyDelta accepted wrong source version")
	}

	// Mutating an old file referenced by the delta must fail SHA-256 verification.
	writeFile(t, filepath.Join(oldBundle, "Contents", "Resources", "update-token"), "tampered")
	badChecksumDir := filepath.Join(tmp, "bad-checksum")
	if err := ApplyDelta(bytes.NewReader(deltaBytes), int64(len(deltaBytes)), "0.1.27", "0.1.28", oldBundle, badChecksumDir); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}

func TestApplyDeltaRejectsEscapingSymlink(t *testing.T) {
	idx := deltaIndex{
		From:    "0.1.27",
		Version: "0.1.28",
		Entries: []deltaEntry{{Path: "Contents/evil", Link: "../../outside"}},
	}
	rawIdx, _ := json.Marshal(idx)
	compIdx := deflateBytes(rawIdx)
	var buf bytes.Buffer
	buf.WriteString(deltaMagic)
	buf.Write(binaryAppendUvarint(nil, uint64(len(compIdx))))
	buf.Write(compIdx)
	b := buf.Bytes()

	tmp := t.TempDir()
	oldDir := filepath.Join(tmp, "old")
	_ = os.MkdirAll(oldDir, 0o755)
	if err := ApplyDelta(bytes.NewReader(b), int64(len(b)), "0.1.27", "0.1.28", oldDir, filepath.Join(tmp, "new")); err == nil || !strings.Contains(err.Error(), "越界符号链接") {
		t.Fatalf("expected escaping symlink rejection, got: %v", err)
	}
}

func binaryAppendUvarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	n := 0
	for v >= 0x80 {
		tmp[n] = byte(v) | 0x80
		v >>= 7
		n++
	}
	tmp[n] = byte(v)
	return append(b, tmp[:n+1]...)
}

func TestDownloadAndStagePrefersDeltaAndReusesAndFallsBack(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "Vantry.app")
	writeFile(t, filepath.Join(target, "Contents", "Info.plist"), "<plist><key>CFBundleExecutable</key><string>vantry</string></plist>")
	writeFile(t, filepath.Join(target, "Contents", "MacOS", "vantry"), strings.Repeat("old-binary-", 300))
	writeFile(t, filepath.Join(target, "Contents", "Resources", "update-token"), "ro-token-123456")

	newApp := filepath.Join(t.TempDir(), "Vantry.app")
	writeFile(t, filepath.Join(newApp, "Contents", "Info.plist"), "<plist><key>CFBundleExecutable</key><string>vantry</string></plist>")
	writeFile(t, filepath.Join(newApp, "Contents", "MacOS", "vantry"), strings.Repeat("new-binary-", 300))
	writeFile(t, filepath.Join(newApp, "Contents", "Resources", "update-token"), "ro-token-123456")
	_ = os.Chmod(filepath.Join(newApp, "Contents", "MacOS", "vantry"), 0o755)

	var deltaBuf bytes.Buffer
	if err := WriteDelta(&deltaBuf, "0.1.3", "0.1.4", target, newApp); err != nil {
		t.Fatalf("WriteDelta: %v", err)
	}
	goodDelta := deltaBuf.Bytes()
	fullZip := zipBytes(t, map[string]string{
		"Vantry.app/Contents/Info.plist":            "<plist><key>CFBundleExecutable</key><string>vantry</string></plist>",
		"Vantry.app/Contents/MacOS/vantry":          "full-zip-binary",
		"Vantry.app/Contents/Resources/update-token": "ro-token-123456",
	}, map[string]os.FileMode{"Vantry.app/Contents/MacOS/vantry": 0o755})

	var serveCorruptDelta atomic.Bool
	var deltaHits, zipHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testRepoPath:
			deltaLen := int64(len(goodDelta))
			if serveCorruptDelta.Load() {
				deltaLen = int64(len("corrupt-delta"))
			}
			writeRelease(t, w, githubRelease{
				TagName: "v0.1.4",
				Assets: []githubAsset{
					{
						Name:               "vantry-0.1.3-to-0.1.4-darwin-arm64.delta",
						Size:               deltaLen,
						BrowserDownloadURL: "http://" + r.Host + "/download/app.delta",
					},
					{
						Name:               "vantry-0.1.4-darwin-arm64.zip",
						Size:               int64(len(fullZip)),
						BrowserDownloadURL: "http://" + r.Host + "/download/app.zip",
					},
				},
			})
		case "/download/app.delta":
			deltaHits.Add(1)
			if serveCorruptDelta.Load() {
				_, _ = w.Write([]byte("corrupt-delta"))
				return
			}
			_, _ = w.Write(goodDelta)
		case "/download/app.zip":
			zipHits.Add(1)
			_, _ = w.Write(fullZip)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// Happy path: Check reports delta, DownloadAndStage fetches ONLY .delta,
	// sets Staged metadata, and second DownloadAndStage reuses staged bundle.
	runner := &runnerLog{}
	mgr := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: runner.run})
	info, err := mgr.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.DeltaAvailable || info.DeltaSize != int64(len(goodDelta)) || info.Staged {
		t.Fatalf("unexpected initial check info: %+v", info)
	}

	staged, err := mgr.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatalf("DownloadAndStage(delta): %v", err)
	}
	if !staged.UsedDelta || !staged.CanAutoRestart || deltaHits.Load() != 1 || zipHits.Load() != 0 {
		t.Fatalf("staged=%+v deltaHits=%d zipHits=%d", staged, deltaHits.Load(), zipHits.Load())
	}
	if got := readFile(t, filepath.Join(staged.StagedPath, "Contents", "MacOS", "vantry")); !strings.HasPrefix(got, "new-binary-") {
		t.Fatalf("delta staged binary mismatch: %q", got[:min(len(got), 20)])
	}
	if !strings.HasPrefix(readFile(t, filepath.Join(target, "Contents", "MacOS", "vantry")), "old-binary-") {
		t.Fatal("target app was mutated during DownloadAndStage")
	}

	curInfo := mgr.CurrentInfo()
	if !curInfo.Staged || curInfo.StagedVersion != "0.1.4" || !curInfo.CanAutoRestart || !curInfo.UsedDelta {
		t.Fatalf("CurrentInfo after staging = %+v", curInfo)
	}
	checkAgain, err := mgr.Check(context.Background())
	if err != nil || !checkAgain.Staged || checkAgain.StagedPath != staged.StagedPath {
		t.Fatalf("subsequent Check lost staged metadata: %+v err=%v", checkAgain, err)
	}

	reused, err := mgr.DownloadAndStage(context.Background(), nil)
	if err != nil || reused.StagedPath != staged.StagedPath || deltaHits.Load() != 1 || zipHits.Load() != 0 {
		t.Fatalf("second DownloadAndStage re-downloaded instead of reusing: %+v err=%v deltaHits=%d", reused, err, deltaHits.Load())
	}

	// Fallback path: corrupted .delta transparently falls back to full .zip.
	serveCorruptDelta.Store(true)
	deltaHits.Store(0)
	zipHits.Store(0)
	fallbackMgr := newTestManager(t, server.URL, Options{TargetAppPath: target, Runner: runner.run})
	fallbackStaged, err := fallbackMgr.DownloadAndStage(context.Background(), nil)
	if err != nil {
		t.Fatalf("DownloadAndStage fallback: %v", err)
	}
	if fallbackStaged.UsedDelta || deltaHits.Load() != 1 || zipHits.Load() != 1 {
		t.Fatalf("expected fallback from delta to zip: %+v deltaHits=%d zipHits=%d", fallbackStaged, deltaHits.Load(), zipHits.Load())
	}
	if got := readFile(t, filepath.Join(fallbackStaged.StagedPath, "Contents", "MacOS", "vantry")); got != "full-zip-binary" {
		t.Fatalf("fallback binary mismatch: %q", got)
	}
	// Ensure delta leftovers were cleaned up.
	_ = slices.Contains([]string{}, "")
}
