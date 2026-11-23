package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func install(t *testing.T, root, id, manifest, source string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		writeFile(t, filepath.Join(dir, "plugin.json"), manifest)
	}
	if source != "" {
		writeFile(t, filepath.Join(dir, "index.ts"), source)
	}
	return dir
}
func installJS(t *testing.T, root, id, manifest, source string) string {
	t.Helper()
	dir := install(t, root, id, manifest, "")
	writeFile(t, filepath.Join(dir, "index.js"), source)
	return dir
}
func buildZipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, value := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

const nativePanel = `{"vantryPanel":{"version":1,"title":"Example","widgets":[{"kind":"text","text":"Ready"}]}}`

func nativeManifest(id string) string {
	return `{"id":"` + id + `","name":"Example","runtime":"native","navigation":[{"title":"Example","page":"home"}],"pages":{"home":` + nativePanel + `}}`
}

func TestOptionalDesktopPluginRunsNativelyAndUninstallsCleanly(t *testing.T) {
	root := t.TempDir()
	host := Host{Directory: filepath.Join(root, "plugins"), DataDir: root}
	if err := host.EnsureBuiltins(); err != nil {
		t.Fatal(err)
	}
	// Clean installs must NOT force-seed vantry-remote-desktop as a builtin plugin.
	list, err := host.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("clean EnsureBuiltins should have 0 forced plugins, got list=%+v err=%v", list, err)
	}
	manifestBytes, err := json.MarshalIndent(desktopManifest(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := install(t, host.Directory, "vantry-remote-desktop", string(manifestBytes), "")
	// Even if a legacy .builtin.json marker existed, EnsureBuiltins clears it.
	if err = writeBuiltinMarker(dir, "0.3.1"); err != nil {
		t.Fatal(err)
	}
	if err = host.EnsureBuiltins(); err != nil {
		t.Fatal(err)
	}
	list, err = host.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	plugin := list[0]
	if plugin.Error != "" || plugin.Builtin || len(plugin.Navigation) != 1 || plugin.Navigation[0].NativePanel != "remote-desktop" {
		t.Fatalf("plugin=%+v", plugin)
	}
	if err = host.SetSetting(context.Background(), plugin.ID, "quality", "ultra"); err != nil {
		t.Fatal(err)
	}
	if quality, _ := host.GetSetting(plugin.ID, "quality"); quality != "ultra" {
		t.Fatal("settings lost", quality)
	}
	if err = host.SetSetting(context.Background(), plugin.ID, "enabled", "false"); err != nil {
		t.Fatal(err)
	}
	list, _ = host.List()
	if !list[0].Disabled || len(list[0].Navigation) != 0 {
		t.Fatalf("disabled plugin still exposes page: %+v", list[0])
	}
	if _, err = host.Run(context.Background(), plugin.ID, nil); err == nil {
		t.Fatal("disabled plugin executed")
	}
	if err = host.SetSetting(context.Background(), plugin.ID, "enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err = host.CallPage(context.Background(), plugin.ID, "desktop", nil); err != nil {
		t.Fatal(err)
	}
	if err = host.RemovePlugin(plugin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(host.Directory, plugin.ID)); !os.IsNotExist(err) {
		t.Fatalf("user-installed remote desktop plugin should be removed on RemovePlugin, stat err=%v", err)
	}
}
func TestLegacyDesktopMigratesInMemoryAndOtherScriptsStayVisible(t *testing.T) {
	root := t.TempDir()
	host := Host{Directory: root}
	original := `{"id":"vantry-remote-desktop","name":"Old desktop","main":"index.ts","version":"0.2.3","settings":[{"key":"defaultQuality","label":"Quality","default":"ultra"}]}`
	dir := install(t, root, "vantry-remote-desktop", original, `throw new Error("must not run")`)
	writeFile(t, filepath.Join(dir, ".origin.json"), `{"repo":"owner/desktop","version":"0.2.3","latestVersion":"0.2.3"}`)
	if err := host.SetSetting(context.Background(), "vantry-remote-desktop", "defaultQuality", "low"); err != nil {
		t.Fatal(err)
	}
	install(t, root, "my-script", `{"id":"my-script","name":"My script","entry":"index.ts"}`, `while(true){}`)
	list, err := host.List()
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	for _, plugin := range list {
		if plugin.ID == "vantry-remote-desktop" {
			if plugin.Error != "" || plugin.Builtin || plugin.HasUpdate {
				t.Fatalf("desktop migration=%+v", plugin)
			}
		} else if !strings.Contains(plugin.Error, "脚本插件已停用") {
			t.Fatalf("script should be diagnosed: %+v", plugin)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "plugin.json")); string(data) != original {
		t.Fatal("user manifest overwritten")
	}
	if quality, _ := host.GetSetting("vantry-remote-desktop", "defaultQuality"); quality != "low" {
		t.Fatal("legacy setting lost")
	}
	if _, err = host.Run(context.Background(), "my-script", nil); err == nil {
		t.Fatal("legacy script executed")
	}
}
func TestDeclarativePageAndFailedUpgradePreserveInstallation(t *testing.T) {
	root := t.TempDir()
	host := Host{Directory: filepath.Join(root, "installed")}
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "plugin.json"), nativeManifest("example"))
	if _, err := host.InstallFromLocal(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	output, err := host.CallPage(context.Background(), "example", "home", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ValidatePanelOutput(output); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "plugin.json"), `{"id":"example","name":"broken","runtime":"native","navigation":[{"title":"missing","page":"missing"}]}`)
	if _, err = host.InstallFromLocal(context.Background(), source); err == nil {
		t.Fatal("invalid page installed")
	}
	if output, err = host.CallPage(context.Background(), "example", "home", nil); err != nil || !strings.Contains(output, "Ready") {
		t.Fatalf("old installation lost: %s %v", output, err)
	}
}
func TestNativeManifestAndFileBoundaries(t *testing.T) {
	root := t.TempDir()
	host := Host{Directory: root}
	install(t, root, "bad", `{"id":"bad","name":"Bad","runtime":"native","pages":{"home":{"vantryPanel":{"version":1,"title":"Invalid","widgets":[{"kind":"unknown"}]}}}}`, "")
	list, err := host.List()
	if err != nil || len(list) != 1 || list[0].Error == "" {
		t.Fatal(list, err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	list, err = host.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, plugin := range list {
		if plugin.ID == "linked" && !strings.Contains(plugin.Error, "符号链接") {
			t.Fatal(plugin)
		}
	}
	if _, err = readPluginFile(root, "../escape", 100); err == nil {
		t.Fatal("path escape accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = host.ListContext(ctx); err == nil {
		t.Fatal("cancellation ignored")
	}
}
func TestZipSlipCannotReplaceNativePlugin(t *testing.T) {
	root := t.TempDir()
	host := Host{Directory: filepath.Join(root, "plugins")}
	archive := filepath.Join(root, "plugin.zip")
	data := buildZipBytes(t, map[string]string{"../escape": "bad", "plugin.json": nativeManifest("example")})
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.InstallFromLocal(context.Background(), archive); err == nil {
		t.Fatal("zip slip accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatal("archive escaped destination")
	}
}

func TestEnsureBuiltinsRetiresDeprecatedScriptsAndHasNoPrivateServerNames(t *testing.T) {
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	dir := installJS(t, pluginsDir, "draft-cleanup", `{"id":"draft-cleanup","name":"整理 JSON 草稿","version":"0.1.0","main":"index.js"}`, `export function run(){}`)
	writeFile(t, filepath.Join(dir, ".origin.json"), `{"repo":"muq9738/vantry-plugin-draft-cleanup","version":"0.1.0"}`)
	host := Host{Directory: pluginsDir, DataDir: root}
	if err := host.EnsureBuiltins(); err != nil {
		t.Fatal(err)
	}
	list, err := host.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("expected 0 forced plugins after retiring unused JS plugin, got %+v (err=%v)", list, err)
	}
	raw, _ := json.Marshal(desktopManifest())
	if strings.Contains(strings.ToLower(string(raw)), "qmt") {
		t.Fatalf("private server keyword found in desktop manifest: %s", raw)
	}
}
