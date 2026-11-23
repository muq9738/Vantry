package desktop

import (
	"context"
	"encoding/json"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/pkg/api"
)

// Metadata queries use the real packaged Core. Commands are counted and
// refused by the test harness so a picker regression cannot invoke an LLM.
type liveCatalogService struct {
	*local.Client
	commands atomic.Int32
}

func (s *liveCatalogService) Dispatch(context.Context, api.Command) (api.Receipt, error) {
	s.commands.Add(1)
	return api.Receipt{}, &api.Error{Code: api.InvalidCommand, Message: "model-picker test forbids execution"}
}

func TestLiveModelPickerBeforeFirstConversation(t *testing.T) {
	binary := os.Getenv("VANTRY_LIVE_CORE_BINARY")
	if binary == "" {
		t.Skip("set VANTRY_LIVE_CORE_BINARY to test the native view with a packaged Core and installed Pi metadata")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := os.MkdirTemp("/tmp", "vantry-live-models-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(temporary)
	data := filepath.Join(temporary, "data")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "core", "--data-dir", data, "--provider", "controlled")
	log, err := os.Create(filepath.Join(temporary, "core.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	defer func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
		if t.Failed() {
			contents, _ := os.ReadFile(log.Name())
			t.Logf("packaged Core diagnostics: %s", contents)
		}
	}()
	service := &liveCatalogService{Client: local.Dial(filepath.Join(data, "core.sock"))}
	defer service.Close()
	limit := time.Now().Add(6 * time.Second)
	for {
		probe, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		health, err := service.Health(probe)
		stop()
		if err == nil && health.State == "ready" {
			break
		}
		if time.Now().After(limit) {
			t.Fatalf("packaged Core did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Threads) != 0 || len(before.Runs) != 0 {
		t.Fatal("not an empty first-run Core")
	}
	w := newWorkbench(service)
	w.defaults.ProviderID = "pi"
	w.defaults.Model = piprovider.ConfiguredModel()
	w.defaults.StatePath = filepath.Join(data, "desktop-state.json")
	if err := w.restoreState(); err != nil {
		t.Fatal(err)
	}
	w.applySnapshot(before)
	tester := ui.NewTester(w.view, 1080, 700)
	if w.modelsErr != "" || len(w.models) == 0 {
		t.Fatalf("real first-run model lookup failed: %q models=%d", w.modelsErr, len(w.models))
	}
	if !tester.HasText("今天想构建什么？") || !w.widgets.modelPickerOpen {
		t.Fatal("first-run model picker did not open on welcome view")
	}
	output := os.Getenv("VANTRY_LIVE_MODEL_ARTIFACTS")
	if output != "" {
		if err := os.MkdirAll(output, 0700); err != nil {
			t.Fatal(err)
		}
		image, err := os.Create(filepath.Join(output, "live-models-offscreen.png"))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(image, tester.Image())
		image.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	selected := w.models[0].Key()
	if preferred := os.Getenv("VANTRY_LIVE_TEST_MODEL"); preferred != "" {
		for _, model := range w.models {
			if model.Key() == preferred {
				selected = model.Key()
				break
			}
		}
	}
	if err := tester.Click("搜索模型"); err != nil {
		t.Fatal(err)
	}
	tester.Type(selected)
	if err := tester.Click(selected); err != nil {
		t.Fatalf("real catalog item was not selectable: %v", err)
	}
	if w.defaults.Model != selected || w.widgets.modelPickerOpen {
		t.Fatalf("selection did not stay in local defaults: model=%q picker=%v", w.defaults.Model, w.widgets.modelPickerOpen)
	}
	after, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if service.commands.Load() != 0 || len(after.Threads) != 0 || len(after.Runs) != 0 || after.Sequence != before.Sequence {
		t.Fatalf("model selection invoked execution: commands=%d threads=%d runs=%d", service.commands.Load(), len(after.Threads), len(after.Runs))
	}
	if output != "" {
		summary := map[string]any{"status": "passed", "view": "native MyGo offscreen view", "core": "packaged executable", "coreBinary": binary, "catalog": "installed Pi registry", "modelCount": len(w.models), "selectedModel": selected, "dispatchCount": service.commands.Load(), "threadCount": 0, "runCount": 0, "llmPromptsSent": 0, "realWindow": "blocked separately by macOS focus"}
		raw, _ := json.MarshalIndent(summary, "", "  ")
		if err := os.WriteFile(filepath.Join(output, "live-model-view.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("packaged Core returned %d real models; native view selected %s with zero commands", len(w.models), selected)
}
