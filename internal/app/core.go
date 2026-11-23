package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/attachments"
	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/adapters/workbench"
	"github.com/muq9738/Vantry/internal/core"
	"github.com/muq9738/Vantry/internal/environments"
	"github.com/muq9738/Vantry/internal/hostplatform"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/internal/plugins"
	"github.com/muq9738/Vantry/pkg/api"
)

// RunCore owns the database and the single-instance lock until shutdown.
func RunCore(ctx context.Context, config Config) error {
	if config.ProviderID == "" {
		config.ProviderID = "pi"
	}
	if config.Model == "" {
		config.Model = piprovider.ConfiguredModel()
	}
	if err := prepareDirectory(config.DataDir); err != nil {
		return err
	}
	lock, err := acquireLock(filepath.Join(config.DataDir, "core.lock"))
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	if err := plugins.EnsureBuiltins(filepath.Join(config.DataDir, "plugins")); err != nil {
		return fmt.Errorf("初始化内置插件：%w", err)
	}
	if err := reclaimSocket(ctx, config.SocketPath()); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	providerOptions := piprovider.Options{
		DataDir: config.DataDir, Executable: executable,
		PiExecutable: config.PiExecutable, SocketPath: config.SocketPath(),
		DefaultProviderID: config.ProviderID, DefaultModel: config.Model,
		DefaultWorkspace: config.Workspace, ControlledDelay: config.Delay,
		// Preserve the user's Pi tools and MCP configuration like the native
		// harness. VANTRY_PI_USER_EXTENSIONS / VANTRY_PI_USER_MCP can opt out.
		EnableUserExtensions: true, EnableUserMCP: true,
	}
	if config.MagpieSettingsPath != "" {
		providerOptions.ResolveModel = func(model string) (string, error) {
			return piprovider.ResolveMagpieModel(config.MagpieSettingsPath, model)
		}
	}
	executor := piprovider.New(providerOptions)
	wbSvc := &workbenchService{config: config, root: ctx}
	federator, err := environments.NewFederator(environments.FederatorOptions{DataDir: config.DataDir, Dial: wbSvc.dialEnvironment})
	if err != nil {
		return fmt.Errorf("打开跨环境编排：%w", err)
	}
	defer federator.Close()
	wbSvc.federator = federator
	routed := &environments.Provider{Local: executor, Federator: federator, ResolveTarget: wbSvc.resolveEnvironmentTarget}
	service, err := core.Open(ctx, core.Options{
		DBPath: filepath.Join(config.DataDir, "state.sqlite"), Provider: routed,
		TelemetryDir:        config.DataDir,
		ValidateAttachments: (attachments.Store{Directory: filepath.Join(config.DataDir, "attachments")}).Validate,
		ForkSession: func(sourceID, targetID string, completedRuns int) error {
			return piprovider.CloneSessionFile(config.DataDir, sourceID, targetID, completedRuns)
		},
		RewindSession: func(threadID string, keepCompletedRuns int) error {
			return piprovider.TruncateSessionFile(config.DataDir, threadID, keepCompletedRuns)
		},
		DefaultProviderID: config.ProviderID, DefaultModel: config.Model,
		DefaultWorkspace: config.Workspace,
	})
	if err != nil {
		return fmt.Errorf("打开 Core：%w", err)
	}
	defer service.Close()
	service.SetModelCatalogProvider(func(ctx context.Context, directory, selected string) (api.ModelCatalog, error) {
		var catalog api.ModelCatalog
		var err error
		piExe := piprovider.ResolveExecutable(config.PiExecutable, config.DataDir)
		if config.MagpieSettingsPath != "" {
			catalog, err = piprovider.MagpieCatalog(ctx, piExe, directory, selected, config.MagpieSettingsPath)
		} else {
			catalog, err = piprovider.Catalog(ctx, piExe, directory, selected)
		}
		if len(catalog.Models) > 0 {
			return catalog, nil
		}
		return catalog, err
	})
	awake := hostplatform.NewKeepAwake(hostplatform.KeepAwakeOptions{})
	defer awake.Close()
	go watchKeepAwake(ctx, service, awake)
	wbSvc.Service = workbench.New(service, executor, config.DataDir)
	wbSvc.ledger = service.UsageLedger()
	defer func() {
		if wbSvc.access != nil {
			wbSvc.access.Close()
		}
		if wbSvc.rd != nil {
			_ = wbSvc.rd.Close()
		}
	}()
	server, err := local.Listen(config.SocketPath(), wbSvc)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Close(closeCtx)
	}()
	if manager, err := wbSvc.accessManager(); err == nil {
		go manager.Restore()
		go func() { _, _ = manager.Discover(ctx) }()
	}
	log.Printf("Vantry Core ready pid=%d socket=%s provider=%s", os.Getpid(), config.SocketPath(), config.ProviderID)
	return server.Serve(ctx)
}

// reclaimSocket runs only while the startup layer owns the instance lock. A
// live compatible service or an ordinary file at the path is never removed.
func reclaimSocket(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket 路径已被其他文件占用：%s", path)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	client := local.Dial(path)
	defer client.Close()
	if _, err := client.Health(probeCtx); err == nil {
		return ErrAlreadyRunning
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("socket 在启动期间发生变化，请重新连接")
	}
	return os.Remove(path)
}

// EnsureCore attaches to an existing healthy Core or starts a detached process.
// The returned client does not own that process and closing it never stops Core.
func EnsureCore(ctx context.Context, config Config, attachOnly bool) (*local.Client, error) {
	if err := prepareDirectory(config.DataDir); err != nil {
		return nil, err
	}
	client := local.Dial(config.SocketPath())
	required := RequiredDesktopCapabilities()
	if err := checkReady(ctx, client); err == nil {
		if result, capErr := EnsureCompatibleCore(ctx, client, HandshakeOptions{RequiredCaps: required, AllowRotate: !attachOnly}); capErr != nil {
			_ = client.Close()
			return nil, capErr
		} else if !result.Rotated {
			return client, nil
		}
	} else if attachOnly {
		_ = client.Close()
		return nil, fmt.Errorf("无法连接 Core，请先运行 vantry core：%w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(config.DataDir, "core.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	logInfo, err := logFile.Stat()
	if err != nil {
		_ = logFile.Close()
		_ = client.Close()
		return nil, err
	}
	arguments := []string{"core", "--data-dir", config.DataDir, "--controlled-delay", config.Delay.String()}
	if config.ProviderID != "" {
		arguments = append(arguments, "--provider", config.ProviderID)
	}
	if config.Model != "" {
		arguments = append(arguments, "--model", config.Model)
	}
	if config.PiExecutable != "" {
		arguments = append(arguments, "--pi-executable", config.PiExecutable)
	}
	if config.MagpieSettingsPath != "" {
		arguments = append(arguments, "--magpie-settings", config.MagpieSettingsPath)
	}
	if config.Workspace != nil {
		arguments = append(arguments, "--workspace-kind", config.Workspace.Kind)
		if config.Workspace.Path != "" {
			arguments = append(arguments, "--workspace", config.Workspace.Path)
		}
		if config.Workspace.Repository != "" {
			arguments = append(arguments, "--repository", config.Workspace.Repository)
		}
		if config.Workspace.Branch != "" {
			arguments = append(arguments, "--branch", config.Workspace.Branch)
		}
		if config.Workspace.BaseRef != "" {
			arguments = append(arguments, "--base-ref", config.Workspace.BaseRef)
		}
	}
	command := exec.Command(executable, arguments...)
	hostprocess.ConfigureDetached(command)
	command.Stdout = logFile
	command.Stderr = logFile
	err = command.Start()
	_ = logFile.Close()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("启动 Core：%w", err)
	}
	// Reap while the desktop remains alive; Core is independent of its context.
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	return waitForCore(ctx, config, client, exited, logInfo.Size())
}

func waitForCore(ctx context.Context, config Config, client *local.Client, exited <-chan error, logOffset int64) (*local.Client, error) {
	required := RequiredDesktopCapabilities()
	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := checkReady(startupCtx, client); err == nil {
			if _, err := VerifyCoreCapabilities(startupCtx, client, required); err != nil {
				_ = client.Close()
				return nil, err
			}
			return client, nil
		}
		select {
		case exitErr := <-exited:
			// Another desktop may have won the startup race. Use its Core only
			// after the same readiness and capability checks as the normal path.
			if err := checkReady(startupCtx, client); err == nil {
				if _, err := VerifyCoreCapabilities(startupCtx, client, required); err == nil {
					return client, nil
				}
			}
			_ = client.Close()
			if exitErr == nil {
				exitErr = errors.New("进程在就绪前退出")
			}
			logPath := filepath.Join(config.DataDir, "core.log")
			detail := coreStartupLog(logPath, logOffset)
			if detail != "" {
				return nil, fmt.Errorf("Core 启动失败：%w\n%s\n日志：%s", exitErr, detail, logPath)
			}
			return nil, fmt.Errorf("Core 启动失败：%w；请查看 %s", exitErr, logPath)
		case <-startupCtx.Done():
			_ = client.Close()
			return nil, fmt.Errorf("等待 Core 就绪失败，请查看 %s：%w", filepath.Join(config.DataDir, "core.log"), startupCtx.Err())
		case <-ticker.C:
		}
	}
}

// Include only output written during this launch, with a bounded diagnostic.
func coreStartupLog(path string, offset int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	const limit = 8 * 1024
	if offset < info.Size()-limit {
		offset = info.Size() - limit
	}
	if offset < 0 {
		offset = 0
	}
	data, _ := io.ReadAll(io.NewSectionReader(file, offset, limit))
	return strings.TrimSpace(string(data))
}

func checkReady(ctx context.Context, client api.Service) error {
	probeCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	health, err := client.Health(probeCtx)
	if err != nil {
		return err
	}
	if health.Version != api.ProtocolVersion {
		return &api.Error{Code: api.VersionMismatch, Message: "Core 协议版本不兼容"}
	}
	if health.State != "ready" {
		return &api.Error{Code: api.NotReady, Message: health.State}
	}
	return nil
}

func watchKeepAwake(ctx context.Context, service api.Service, awake *hostplatform.KeepAwake) {
	snap, err := service.Snapshot(ctx)
	if err == nil {
		awake.SetActive(api.ActiveRunCount(snap) > 0)
	}
	sub, err := service.Subscribe(ctx, snap.Sequence)
	if err != nil {
		return
	}
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-sub.Events:
			if !ok {
				return
			}
			if cur, err := service.Snapshot(ctx); err == nil {
				awake.SetActive(api.ActiveRunCount(cur) > 0)
			}
		case <-sub.Errors:
			return
		}
	}
}
