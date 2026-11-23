package app

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/muq9738/Vantry/internal/desktop"
	"github.com/muq9738/Vantry/internal/desktoprelay"
	"github.com/muq9738/Vantry/internal/plugins"
	"github.com/muq9738/vantry-plugin-remote-desktop/rtc"
	"time"
)

type rtcViewer interface {
	OnFrame(func())
	LatestFrame() (*image.RGBA, uint64, rtc.Stats)
	SendMouse(string, float64, float64, int) error
	SendWheel(float64, float64) error
	SendKey(string, int, string, uint32) error
	SetQuality(string) error
	Done() <-chan struct{}
	Err() error
	Close() error
}
type rtcHost interface {
	Ticket() string
	SessionID() string
	Connected() bool
	Err() error
	Close() error
}

// The RTC engine retains the last peer error. A viewer's normal "bye" closes
// that peer, not the explicitly enabled host listener or sharing permission.
func desktopHostError(host rtcHost) error {
	err := host.Err()
	if errors.Is(err, rtc.ErrClosed) {
		return nil
	}
	return err
}

type ownedViewer struct {
	*rtc.ClientSession
	relay interface{ Close() }
}

func (v *ownedViewer) Close() error { err := v.ClientSession.Close(); v.relay.Close(); return err }

type ownedHost struct {
	*rtc.Host
	relay interface{ Close() }
}

func (h *ownedHost) Close() error { err := h.Host.Close(); h.relay.Close(); return err }
func (h *ownedHost) Ticket() string {
	if relay, ok := h.relay.(*desktoprelay.HTTPRelay); ok {
		return relay.Ticket()
	}
	return h.Host.Ticket()
}

type RemoteDesktopOptions struct{ DataDir string }

// RemoteDesktopBridge uses only the RTC plugin. Viewers own local decode;
// host methods are called by the owning Core's rd.* API, never by a foreign view.
type RemoteDesktopBridge struct {
	rootCtx        context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	mu             sync.Mutex
	session        rtcViewer
	generation     uint64
	closed         bool
	inputError     string
	dataDir        string
	desktopPeers   []desktopPeer
	hostMu         sync.Mutex
	host           rtcHost
	hostGeneration uint64
	hostStarting   bool
	hostRequestID  string
	hostCancel     context.CancelFunc
	relayHost      string

	dial      func(context.Context, rtc.ConnectConfig) (rtcViewer, error)
	startHost func(context.Context, rtc.HostConfig) (rtcHost, error)
	helper    func() (string, error)
}

func NewRemoteDesktopDriver(ctx context.Context, options ...RemoteDesktopOptions) *RemoteDesktopBridge {
	if ctx == nil {
		ctx = context.Background()
	}
	life, cancel := context.WithCancel(ctx)
	b := &RemoteDesktopBridge{rootCtx: life, cancel: cancel, helper: remoteDesktopHelperPath}
	if len(options) > 0 {
		b.dataDir = options[0].DataDir
	}
	b.dial = func(ctx context.Context, cfg rtc.ConnectConfig) (rtcViewer, error) {
		if strings.HasPrefix(strings.TrimSpace(cfg.Ticket), "vantry-lan:") {
			return rtc.Dial(ctx, cfg)
		}
		if strings.HasPrefix(strings.TrimSpace(cfg.Ticket), "vantry://desktop") {
			ticket, err := desktoprelay.ParseTicket(cfg.Ticket)
			if err != nil {
				return nil, err
			}
			relay, err := desktoprelay.NewClient(ticket.Relay, "", true)
			if err != nil {
				return nil, err
			}
			cfg.Relay = relay
			cfg.SSHHost = ticket.Relay
			cfg.Ticket = (rtc.Ticket{SSHHost: "vantry-relay", SessionID: ticket.SessionID, Token: ticket.Token}).String()
			viewer, err := rtc.Dial(ctx, cfg)
			if err != nil {
				relay.Close()
				return nil, err
			}
			return &ownedViewer{viewer, relay}, nil
		}
		// Existing saved SSH tickets remain usable during migration.
		configFile, err := b.localSSHConfigFile()
		if err != nil {
			return nil, err
		}
		relay, err := rtc.NewSSHRelay(rtc.SSHConfig{Host: cfg.SSHHost, ConfigFile: configFile})
		if err != nil {
			return nil, err
		}
		cfg.Relay = relay
		viewer, err := rtc.Dial(ctx, cfg)
		if err != nil {
			relay.Close()
			return nil, err
		}
		return &ownedViewer{viewer, relay}, nil
	}
	b.startHost = func(ctx context.Context, cfg rtc.HostConfig) (rtcHost, error) {
		if strings.TrimSpace(cfg.SSHHost) == "" {
			return rtc.StartHost(ctx, cfg)
		}
		if strings.Contains(cfg.SSHHost, "://") {
			life, cancel := context.WithCancel(ctx)
			local := cfg
			local.SSHHost = ""
			direct, err := rtc.StartHost(life, local)
			if err != nil {
				cancel()
				return nil, err
			}
			return &automaticDesktopHost{direct: direct, ctx: life, cancel: cancel, create: func(ctx context.Context) (rtcHost, error) {
				key, err := (plugins.Host{Directory: filepath.Join(b.dataDir, "plugins"), DataDir: b.dataDir}).GetSetting("vantry-remote-desktop", "relayKey")
				if err != nil {
					return nil, err
				}
				relay, err := desktoprelay.NewClient(cfg.SSHHost, key, false)
				if err != nil {
					return nil, err
				}
				fallback := cfg
				fallback.Relay = relay
				host, err := rtc.StartHost(ctx, fallback)
				if err != nil {
					relay.Close()
					return nil, err
				}
				return &ownedHost{host, relay}, nil
			}}, nil
		}
		configFile, err := b.localSSHConfigFile()
		if err != nil {
			return nil, err
		}
		relay, err := rtc.NewSSHRelay(rtc.SSHConfig{Host: cfg.SSHHost, ConfigFile: configFile})
		if err != nil {
			return nil, err
		}
		cfg.Relay = relay
		host, err := rtc.StartHost(ctx, cfg)
		if err != nil {
			relay.Close()
			return nil, err
		}
		return &ownedHost{host, relay}, nil
	}
	return b
}

// This resolver is anchored to the physical process's own App data directory.
// A remote view cannot supply a foreign Core's path to the local SSH client.
func (b *RemoteDesktopBridge) localSSHConfigFile() (string, error) {
	path := strings.TrimSpace(os.Getenv("VANTRY_REMOTE_SSH_CONFIG"))
	if path == "" && b.dataDir != "" {
		var err error
		path, err = (plugins.Host{Directory: filepath.Join(b.dataDir, "plugins")}).GetSetting("vantry-remote-desktop", "sshConfigFile")
		if err != nil {
			return "", err
		}
		path = strings.TrimSpace(path)
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("SSH 配置文件需要本机绝对路径")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("本机 SSH 配置文件不存在或不可读")
	}
	return path, nil
}

func remoteDesktopHelperPath() (string, error) {
	check := func(path string) (string, error) {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0111 == 0) {
			return "", errors.New("原生 H.264 helper 未打包或不可执行，请安装配套的 Vantry 应用")
		}
		return path, nil
	}
	if explicit := strings.TrimSpace(os.Getenv("VANTRY_REMOTE_HELPER")); explicit != "" {
		return check(explicit)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	name := "vantry-remote-helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	packaged := filepath.Join(filepath.Dir(exe), "remote-desktop", name)
	if runtime.GOOS == "darwin" {
		packaged = filepath.Clean(filepath.Join(filepath.Dir(exe), "..", "Resources", "remote-desktop", name))
	}
	if path, err := check(packaged); err == nil {
		return path, nil
	}
	return check(filepath.Join("resources", runtime.GOOS+"-"+runtime.GOARCH, "remote-desktop", name))
}

func (b *RemoteDesktopBridge) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.cancel()
		_ = b.Disconnect()
		_, _ = b.HostStop(context.Background())
	})
	return nil
}

func (b *RemoteDesktopBridge) Connect(ctx context.Context, cfg desktop.RemoteDesktopConnect, onFrame func()) error {
	native := strings.HasPrefix(strings.TrimSpace(cfg.Target), "vantry://desktop") || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(cfg.Target)), "DESK-")
	if native {
		relayURL := cfg.RelayURL
		if relayURL == "" {
			relayURL = cfg.RelaySSHHost
		}
		ticket, err := b.desktopPair(ctx, relayURL, cfg.Target)
		if err != nil {
			return err
		}
		cfg.Target = ticket.Link()
	}
	lan := strings.HasPrefix(strings.TrimSpace(cfg.Target), "vantry-lan:")
	if lan {
		if _, err := rtc.ParseLANTicket(cfg.Target); err != nil {
			return err
		}
		if cfg.RelayOnly {
			return errors.New("这是局域网连接地址，请选择局域网连接")
		}
	} else if !native {
		if _, err := rtc.ParseTicket(cfg.Target); err != nil {
			return err
		}
		if cfg.DirectOnly || strings.TrimSpace(cfg.RelaySSHHost) == "" {
			return errors.New("跨网络连接需配置你自己的中继；未配置时仅支持局域网连接")
		}
	}
	helper, err := b.helper()
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errors.New("远控视图已经关闭")
	}
	b.generation++
	generation := b.generation
	old := b.session
	b.session = nil
	b.inputError = ""
	b.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	client, err := b.dial(ctx, rtc.ConnectConfig{Ticket: strings.TrimSpace(cfg.Target), HelperPath: helper, SSHHost: strings.TrimSpace(cfg.RelaySSHHost)})
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed || generation != b.generation || ctx.Err() != nil {
		b.mu.Unlock()
		_ = client.Close()
		return context.Canceled
	}
	b.session = client
	b.mu.Unlock()
	client.OnFrame(func() {
		b.mu.Lock()
		active := b.session == client && b.generation == generation && !b.closed
		b.mu.Unlock()
		if active && onFrame != nil {
			onFrame()
		}
	})
	if cfg.Quality != "" {
		b.rememberInputError(client, client.SetQuality(cfg.Quality))
	}
	return nil
}

func (b *RemoteDesktopBridge) Disconnect() error {
	b.mu.Lock()
	b.generation++
	client := b.session
	b.session = nil
	b.inputError = ""
	b.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}
func (b *RemoteDesktopBridge) viewer() rtcViewer { b.mu.Lock(); defer b.mu.Unlock(); return b.session }
func (b *RemoteDesktopBridge) rememberInputError(client rtcViewer, err error) {
	if err != nil {
		b.mu.Lock()
		if b.session == client {
			b.inputError = err.Error()
		}
		b.mu.Unlock()
	}
}
func (b *RemoteDesktopBridge) LatestFrame() (*image.RGBA, uint64, desktop.RemoteDesktopStats) {
	client := b.viewer()
	if client == nil {
		return nil, 0, desktop.RemoteDesktopStats{Codec: "h264"}
	}
	frame, seq, stats := client.LatestFrame()
	connected := stats.Connected
	select {
	case <-client.Done():
		connected = false
	default:
	}
	errText := stats.Error
	if err := client.Err(); err != nil {
		errText = err.Error()
	}
	b.mu.Lock()
	if b.inputError != "" {
		errText = b.inputError
	}
	b.mu.Unlock()
	return frame, seq, desktop.RemoteDesktopStats{Connected: connected, Mode: stats.Mode, Transport: stats.Transport, Codec: stats.Codec, Hardware: stats.Hardware, RelayHost: stats.RelayHost,
		Width: stats.Width, Height: stats.Height, FPS: int(stats.FPS + .5), RTTMs: int(stats.RTTMs + .5), Kbps: int(stats.Kbps + .5), FirstFrameMs: stats.FirstFrameMs,
		FrameAgeMs: stats.FrameAgeMs, FrameAgeP50Ms: stats.FrameAgeP50Ms, FrameAgeP95Ms: stats.FrameAgeP95Ms, ClockSynchronized: stats.ClockSynchronized,
		LocalCandidateType: stats.LocalCandidateType, RemoteCandidateType: stats.RemoteCandidateType, CandidateProtocol: stats.CandidateProtocol, Error: errText}
}
func (b *RemoteDesktopBridge) SendMouse(kind string, x, y float64, button int) {
	if client := b.viewer(); client != nil {
		b.rememberInputError(client, client.SendMouse(kind, x, y, button))
	}
}
func (b *RemoteDesktopBridge) SendWheel(dx, dy float64) {
	if client := b.viewer(); client != nil {
		b.rememberInputError(client, client.SendWheel(dx, dy))
	}
}
func (b *RemoteDesktopBridge) SendKey(kind string, key int, text string, mods uint32) {
	if client := b.viewer(); client != nil {
		b.rememberInputError(client, client.SendKey(kind, key, text, mods))
	}
}
func (b *RemoteDesktopBridge) SetQuality(quality string) {
	if client := b.viewer(); client != nil {
		b.rememberInputError(client, client.SetQuality(quality))
	}
}

func (b *RemoteDesktopBridge) HostStart(ctx context.Context, sshHost string) (desktop.RemoteDesktopHostStatus, error) {
	return b.HostStartOwned(ctx, sshHost, "")
}

// requestID scopes cancellation cleanup, not permission or authentication.
func (b *RemoteDesktopBridge) HostStartOwned(ctx context.Context, sshHost, requestID string) (desktop.RemoteDesktopHostStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return desktop.RemoteDesktopHostStatus{}, err
	}
	if enabled, _ := (plugins.Host{Directory: filepath.Join(b.dataDir, "plugins"), DataDir: b.dataDir}).GetSetting("vantry-remote-desktop", "enabled"); enabled == "false" {
		return desktop.RemoteDesktopHostStatus{}, errors.New("远程桌面插件已停用")
	}
	alias := strings.TrimSpace(sshHost)
	if strings.Contains(alias, "://") {
		if _, err := desktoprelay.ValidateURL(alias); err != nil {
			return desktop.RemoteDesktopHostStatus{}, err
		}
	}
	helper, err := b.helper()
	if err != nil {
		return desktop.RemoteDesktopHostStatus{}, err
	}
	b.hostMu.Lock()
	if b.host != nil {
		if requestID != "" {
			b.hostRequestID = requestID
		}
		b.hostMu.Unlock()
		return b.HostStatus(ctx)
	}
	if b.hostStarting {
		b.hostMu.Unlock()
		return desktop.RemoteDesktopHostStatus{}, errors.New("被控服务正在启动")
	}
	if b.rootCtx.Err() != nil {
		b.hostMu.Unlock()
		return desktop.RemoteDesktopHostStatus{}, context.Canceled
	}
	b.hostGeneration++
	generation := b.hostGeneration
	b.hostStarting = true
	b.hostRequestID = requestID
	life, cancel := context.WithCancel(b.rootCtx)
	b.hostCancel = cancel
	b.hostMu.Unlock()
	stopWatch := context.AfterFunc(ctx, func() {
		b.hostMu.Lock()
		if b.hostStarting && b.hostGeneration == generation {
			cancel()
		}
		b.hostMu.Unlock()
	})
	quality, _ := (plugins.Host{Directory: filepath.Join(b.dataDir, "plugins"), DataDir: b.dataDir}).GetSetting("vantry-remote-desktop", "quality")
	if quality == "high" {
		quality = "ultra"
	}
	if quality == "" {
		quality = "balanced"
	}
	host, err := b.startHost(life, rtc.HostConfig{SSHHost: alias, HelperPath: helper, Quality: quality})
	stopWatch()
	b.hostMu.Lock()
	if b.hostGeneration != generation || life.Err() != nil {
		if b.hostGeneration == generation {
			b.hostStarting = false
			b.hostCancel = nil
			b.hostRequestID = ""
		}
		b.hostMu.Unlock()
		if host != nil {
			_ = host.Close()
		}
		cancel()
		return desktop.RemoteDesktopHostStatus{}, context.Canceled
	}
	b.hostStarting = false
	if err != nil {
		b.hostCancel = nil
		b.hostRequestID = ""
		b.hostMu.Unlock()
		cancel()
		return desktop.RemoteDesktopHostStatus{}, err
	}
	b.host = host
	b.relayHost = alias
	b.hostMu.Unlock()
	return b.HostStatus(ctx)
}
func (b *RemoteDesktopBridge) HostStop(ctx context.Context) (desktop.RemoteDesktopHostStatus, error) {
	return b.HostStopOwned(ctx, "")
}
func (b *RemoteDesktopBridge) HostStopOwned(ctx context.Context, requestID string) (desktop.RemoteDesktopHostStatus, error) {
	b.hostMu.Lock()
	if requestID != "" && requestID != b.hostRequestID {
		b.hostMu.Unlock()
		return b.HostStatus(ctx)
	}
	b.hostGeneration++
	host, cancel := b.host, b.hostCancel
	b.host = nil
	b.hostCancel = nil
	b.hostStarting = false
	b.hostRequestID = ""
	b.relayHost = ""
	b.hostMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if host != nil {
		_ = host.Close()
	}
	return desktop.RemoteDesktopHostStatus{}, nil
}
func (b *RemoteDesktopBridge) HostStatus(ctx context.Context) (desktop.RemoteDesktopHostStatus, error) {
	b.hostMu.Lock()
	host, relay, requestID := b.host, b.relayHost, b.hostRequestID
	b.hostMu.Unlock()
	if host == nil {
		return desktop.RemoteDesktopHostStatus{}, nil
	}
	if err := desktopHostError(host); err != nil {
		return desktop.RemoteDesktopHostStatus{}, err
	}
	clients := 0
	if host.Connected() {
		clients = 1
	}
	status := desktop.RemoteDesktopHostStatus{Hosting: true, RelaySSHHost: relay, ConnectURL: host.Ticket(), Clients: clients, StartRequestID: requestID}
	nativeHost := host
	if automatic, ok := host.(*automaticDesktopHost); ok {
		nativeHost = automatic.currentRelay()
	}
	if native, ok := nativeHost.(*ownedHost); ok {
		if relay, ok := native.relay.(*desktoprelay.HTTPRelay); ok {
			pairing := relay.Pairing()
			status.ShareCode = pairing.Code
			status.ShareExpiresAt = pairing.ExpiresAt
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			status.PendingViewer, _ = relay.Pending(probe)
		}
	}
	if lan, ok := host.(interface {
		DirectAddrs() []string
		ListenAddr() string
	}); ok {
		status.DirectAddrs, status.ListenAddr = lan.DirectAddrs(), lan.ListenAddr()
	}
	return status, nil
}

func (b *RemoteDesktopBridge) HostApprove(ctx context.Context, allow bool) (desktop.RemoteDesktopHostStatus, error) {
	b.hostMu.Lock()
	current := b.host
	b.hostMu.Unlock()
	if automatic, ok := current.(*automaticDesktopHost); ok {
		current = automatic.currentRelay()
	}
	host, ok := current.(*ownedHost)
	if !ok {
		return desktop.RemoteDesktopHostStatus{}, errors.New("当前共享不使用 Vantry 配对中继")
	}
	relay, ok := host.relay.(*desktoprelay.HTTPRelay)
	if !ok {
		return desktop.RemoteDesktopHostStatus{}, errors.New("当前共享不支持配对确认")
	}
	if err := relay.Approve(ctx, allow); err != nil {
		return desktop.RemoteDesktopHostStatus{}, err
	}
	return b.HostStatus(ctx)
}
func (b *RemoteDesktopBridge) HostCode(ctx context.Context) (desktop.RemoteDesktopHostStatus, error) {
	b.hostMu.Lock()
	current := b.host
	b.hostMu.Unlock()
	if automatic, ok := current.(*automaticDesktopHost); ok {
		var err error
		current, err = automatic.relay(ctx)
		if err != nil {
			return desktop.RemoteDesktopHostStatus{}, err
		}
	}
	host, ok := current.(*ownedHost)
	if !ok {
		return desktop.RemoteDesktopHostStatus{}, errors.New("请先开启经中继的桌面共享")
	}
	relay, ok := host.relay.(*desktoprelay.HTTPRelay)
	if !ok {
		return desktop.RemoteDesktopHostStatus{}, errors.New("旧连接请迁移至 HTTPS 桌面中继")
	}
	if _, err := relay.GenerateCode(ctx); err != nil {
		return desktop.RemoteDesktopHostStatus{}, err
	}
	return b.HostStatus(ctx)
}

var _ desktop.RemoteDesktopDriver = (*RemoteDesktopBridge)(nil)
var _ desktop.RemoteDesktopHoster = (*RemoteDesktopBridge)(nil)
