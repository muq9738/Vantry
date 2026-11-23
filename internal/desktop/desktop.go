package desktop

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/muq9738/Vantry/internal/desktop/webcanvas"
	"github.com/muq9738/Vantry/pkg/api"
)

// Options supplies defaults for new threads; committed threads keep their
// persisted provider, model, workspace and approval configuration. StatePath
// is opt-in: the parent passes DataDir/desktop-state.json when UI preferences
// and safe local drafts should survive a restart.
type Options struct {
	ProviderID         string
	Model              string
	Workspace          *api.WorkspaceSpec
	ApprovalPolicy     string
	StatePath          string
	RemoteHost         string
	RemoteName         string
	RemotePeer         bool
	ConnectionMode     func() string
	ConnectEnvironment func(context.Context, string) (*EnvironmentSession, error)
	// Version is this build's version, shown in Settings beside the update
	// check; empty until the launcher passes it.
	Version string
	// RemoteDesktop streams another machine's screen into the main stage.
	// Nil keeps the view, which then reports that no engine is linked;
	// hosting this Mac falls back to Core's rd.* workbench actions.
	RemoteDesktop RemoteDesktopDriver
	// Each environment view gets its own local decoder/session. Hosting is
	// controlled through the Core that owns that environment, never this factory.
	RemoteDesktopFactory func(context.Context) RemoteDesktopDriver
}

// Run opens the native workbench and serves it until its window closes or ctx
// is cancelled. Call it from the OS main thread; it owns MyGo's event loop when
// nothing else has started it, and otherwise waits for its own window to go.
//
// The window reaches Core only through service. Closing it releases this
// client's work - the subscription and this client's own calls - and never stops
// Core, whose lifetime belongs to the process that started it.
func Run(ctx context.Context, service api.Service) error {
	return RunWithOptions(ctx, service, Options{})
}

func RunWithOptions(ctx context.Context, service api.Service, options Options) error {
	if ctx == nil {
		return errors.New("desktop: Run needs a context")
	}
	if service == nil {
		return errors.New("desktop: Run needs an api.Service")
	}
	w := newWorkbench(service)
	if options.ProviderID == "" {
		options.ProviderID = "pi"
	}
	w.defaults = options
	w.defaults.Workspace = cloneWorkspace(options.Workspace)
	w.defaults.ApprovalPolicy = normalizeApprovalPolicy(options.ApprovalPolicy)
	if err := w.restoreState(); err != nil {
		w.stateNotice = notice{Text: "无法恢复上次界面状态；已使用安全默认配置。", Level: noticeWarn}
	}
	h := newHost(ctx, w)

	mygo.App.OnSecondInstance(func(_ []string, _ string) {
		h.mu.Lock()
		window := h.window
		h.mu.Unlock()
		if window != nil {
			window.Show()
			window.Focus()
		}
	})
	_ = webcanvas.RegisterProtocol()
	mygo.App.WhenReady(h.open)
	stopWatching := make(chan struct{})
	go h.watchContext(stopWatching)

	err := mygo.App.Run()
	close(stopWatching)
	switch {
	case err == nil:
	case strings.Contains(err.Error(), "App.Run called twice"):
		// Another component owns the event loop: wait for this window instead of
		// stopping the application.
		<-h.finished
	default:
		return err
	}
	return ctx.Err()
}

// host ties the workbench to one MyGo window and to the client context that
// bounds this client's calls.
type host struct {
	w      *workbench
	resync *resync
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	window       *mygo.Window
	snapshotter  *webcanvas.Snapshotter
	closing      bool
	finished     chan struct{}
	once         sync.Once
	environments *environmentHub

	// quit stops the application loop. It is a field so a test can check what
	// closing this client releases without starting a window.
	quit func()
}

func newHost(parent context.Context, w *workbench) *host {
	clientCtx, cancel := context.WithCancel(parent)
	h := &host{
		w:        w,
		resync:   newResync(),
		ctx:      clientCtx,
		cancel:   cancel,
		finished: make(chan struct{}),
		quit:     mygo.App.Quit,
	}
	w.ctx = clientCtx
	w.quit = func() {
		h.clientStopped()
		h.quit()
	}
	w.initializeRemoteDesktop()
	h.environments = newEnvironmentHub(w, clientCtx)
	return h
}

// open runs on the main thread once the application is ready: it hands the
// workbench the window it posts to, then starts the event worker.
func (h *host) open() {
	h.mu.Lock()
	if h.closing || h.ctx.Err() != nil {
		h.closing = true
		h.mu.Unlock()
		h.cancel()
		h.finish()
		h.quit()
		return
	}
	win := mygo.NewWindow(mygo.WindowOptions{
		Title:                "Vantry",
		Width:                1280,
		Height:               820,
		MinWidth:             640,
		MinHeight:            420,
		TitleBarStyle:        mygo.TitleBarHidden,
		TrafficLightPosition: &mygo.Point{X: 16, Y: 16},
		Content:              ui.View(func(c *ui.Context) { h.environments.current.view(c) }),
	})
	h.window = win
	h.snapshotter = webcanvas.NewSnapshotter(func(string) { win.Update(func() {}) })
	h.snapshotter.EnableLive()
	h.w.window = win
	h.w.webSnapshotter = h.snapshotter
	h.mu.Unlock()

	// State changes from the network go through the window, which builds the
	// next frame on the main thread; RPCs leave the main thread entirely.
	h.w.post = win.Update
	win.OnFocus(func() {
		win.Update(func() {
			if h.environments.current != nil {
				h.environments.current.remotePermissionFocusReturned()
			}
		})
	})
	h.w.start = func(fn func()) { go fn() }
	h.w.pickDirectory = func() { h.w.openRemoteDirectory("") }
	h.w.nativeDirectoryPicker = func() {
		go func() {
			paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: win, Title: "选择项目目录", ButtonLabel: "选择项目", Directory: true})
			win.Update(func() {
				if err != nil {
					h.w.stateNotice = notice{Text: "无法打开项目选择器：" + err.Error(), Level: noticeError}
				} else if len(paths) > 0 {
					h.w.configureWorkspace(paths[0])
					h.w.widgets.composerFocus.Focus()
				}
			})
		}()
	}

	h.environments.bind = func(target *workbench) {
		target.post = win.Update
		target.start = func(fn func()) { go fn() }
		target.quit = h.w.quit
		target.window = win
		target.webSnapshotter = h.snapshotter
		target.pickDirectory = func() { target.openRemoteDirectory("") }
		if target.defaults.ProviderID != "controlled" {
			target.ensureModels(false)
		}
		target.loadPlugins()
		go target.syncWorker(target.ctx, newResync())
	}
	h.environments.load()
	win.OnClosed(func() { h.clientStopped() })
	if h.w.defaults.ProviderID != "controlled" {
		h.w.ensureModels(false)
	}
	h.w.loadPlugins()

	go h.w.syncWorker(h.ctx, h.resync)
}

// clientStopped releases this client's activity: the event worker returns, its
// subscription closes, in-flight calls of its own are cancelled.
func (h *host) clientStopped() {
	h.w.visuals.destroyInlineCanvases()
	if h.snapshotter != nil {
		h.snapshotter.Close()
	}
	h.environments.close()
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
	h.cancel()
	h.finish()
}

func (h *host) finish() { h.once.Do(func() { close(h.finished) }) }

// watchContext closes the window when the caller cancels ctx, so Run returns to
// whoever asked for it.
func (h *host) watchContext(stop <-chan struct{}) {
	select {
	case <-h.ctx.Done():
		h.shutdown()
	case <-stop:
	}
}

func (h *host) shutdown() {
	h.mu.Lock()
	h.closing = true
	win := h.window
	h.mu.Unlock()
	// Whatever the reason, this client's calls are over.
	h.cancel()
	if win == nil {
		// The window never opened, so nothing else will ask the loop to stop.
		h.finish()
		h.quit()
		return
	}
	win.Close()
}
