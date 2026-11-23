package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// ApplyOptions configures waiting for a quitting desktop process during
// one-click update application.
type ApplyOptions struct {
	WaitPID     int
	WaitTimeout time.Duration
}

// prepareInstaller creates a user-invoked command, not a startup service. It runs
// the downloaded bundle's installer mode, so no running target file is needed.
func prepareInstaller(root, bundle, target, dataDir string) (string, error) {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := "#!/bin/sh\nset -eu\nbundle=" + quote(bundle) + "\n" +
		"exe=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' \"$bundle/Contents/Info.plist\")\n" +
		"exec \"$bundle/Contents/MacOS/$exe\" apply-update --staged \"$bundle\" --target " + quote(target) + " --data-dir " + quote(dataDir) + "\n"
	path := filepath.Join(root, "安装更新.command")
	if err := os.WriteFile(path, []byte(command), 0o700); err != nil {
		return "", fmt.Errorf("无法准备退出后安装命令: %w", err)
	}
	return path, nil
}

type appProcess struct {
	pid     int
	command string
}

func appProcesses(ctx context.Context, target string) ([]appProcess, error) {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil, errors.New("无法确认旧 Vantry 进程是否已退出，未替换应用")
	}
	var processes []appProcess
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0]))
		if strings.HasPrefix(command, target+string(filepath.Separator)) {
			processes = append(processes, appProcess{pid, command})
		}
	}
	return processes, nil
}

// Apply installs only after all processes from the target bundle have exited.
// An idle detached Core may be drained through its existing local API; active
// tasks refuse shutdown, and a running desktop must be closed by the user.
func (m *Manager) Apply(ctx context.Context, staged, target, dataDir string) error {
	return m.applyOffline(ctx, staged, target, dataDir, appProcesses)
}

// ApplyWithOptions installs a staged bundle, optionally waiting up to
// opts.WaitTimeout for an exiting desktop process (WaitPID / non-Core target
// processes) before draining an idle Core and swapping the bundle.
func (m *Manager) ApplyWithOptions(ctx context.Context, staged, target, dataDir string, opts ApplyOptions) error {
	return m.applyOfflineWithOptions(ctx, staged, target, dataDir, opts, appProcesses)
}

func (m *Manager) applyOffline(ctx context.Context, staged, target, dataDir string, processes func(context.Context, string) ([]appProcess, error)) error {
	return m.applyOfflineWithOptions(ctx, staged, target, dataDir, ApplyOptions{}, processes)
}

func (m *Manager) applyOfflineWithOptions(ctx context.Context, staged, target, dataDir string, opts ApplyOptions, processes func(context.Context, string) ([]appProcess, error)) error {
	ctx = contextOrBackground(ctx)
	if !filepath.IsAbs(staged) || !filepath.IsAbs(target) || filepath.Clean(staged) == filepath.Clean(target) ||
		!strings.HasSuffix(staged, ".app") || !strings.HasSuffix(target, ".app") || within(target, staged) || within(staged, target) {
		return errors.New("更新必须使用独立暂存的 .app 和原安装路径")
	}
	if err := m.verifyBundle(ctx, staged); err != nil {
		return err
	}
	if opts.WaitTimeout > 0 {
		waitDesktop, cancelWait := context.WithTimeout(ctx, opts.WaitTimeout)
		defer cancelWait()
		for {
			live, err := processes(waitDesktop, target)
			if err != nil {
				return err
			}
			if len(live) == 0 {
				break
			}
			onlyIdleCore := false
			if dataDir != "" {
				if status, statusErr := offlineCoreStatus(waitDesktop, dataDir); statusErr == nil {
					onlyIdleCore = true
					for _, p := range live {
						if p.pid != status.PID || (opts.WaitPID > 0 && p.pid == opts.WaitPID) {
							onlyIdleCore = false
							break
						}
					}
				}
			}
			if onlyIdleCore {
				break
			}
			select {
			case <-waitDesktop.Done():
				return errors.New("Vantry 窗口未在规定时间内退出，更新未安装")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	live, err := processes(ctx, target)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		if dataDir == "" {
			return errors.New("请先完整退出 Vantry 及其 Core，再运行安装命令")
		}
		status, err := offlineCoreStatus(ctx, dataDir)
		if err != nil {
			return fmt.Errorf("无法核对运行中的 Core，未替换应用: %w", err)
		}
		for _, p := range live {
			if p.pid != status.PID {
				return errors.New("请先退出 Vantry 窗口和其他使用此应用的进程，再运行安装命令")
			}
		}
		if status.State != "ready" || status.ActiveRuns != 0 {
			return errors.New("Core 仍有任务或未就绪，更新未安装，请等待任务结束")
		}
		if err := drainOfflineCore(ctx, dataDir); err != nil {
			return err
		}
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for {
			live, err = processes(wait, target)
			if err != nil {
				return err
			}
			if len(live) == 0 {
				break
			}
			select {
			case <-wait.Done():
				return errors.New("旧 Core 尚未退出，未替换应用")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	// Recheck immediately before the only path that renames the target.
	live, err = processes(ctx, target)
	if err != nil {
		return err
	}
	if len(live) > 0 {
		return errors.New("Vantry 已重新启动，更新未安装，请先退出")
	}
	if err := swapApp(staged, target); err != nil {
		return err
	}
	return nil
}

// LaunchApply verifies the staged bundle and launches its detached apply-update
// worker so the caller (desktop UI) can exit and let installation proceed.
func (m *Manager) LaunchApply(ctx context.Context, version string, desktopPID int) (StagedUpdate, error) {
	ctx = contextOrBackground(ctx)
	m.mu.Lock()
	var staged StagedUpdate
	hasStaged := m.staged != nil
	if hasStaged {
		staged = *m.staged
	}
	dataDir := m.opts.DataDir
	m.mu.Unlock()

	if !hasStaged || !pathExists(staged.StagedPath) {
		return StagedUpdate{}, errors.New("请先下载更新")
	}
	if version != "" && normalizeVersion(version) != normalizeVersion(staged.Version) {
		return StagedUpdate{}, fmt.Errorf("已暂存版本为 %s，与请求安装的 %s 不一致", staged.Version, version)
	}
	if !m.canAutoRestart(staged) {
		return StagedUpdate{}, errors.New("当前安装环境不支持自动重启替换，请退出 Vantry 后手动安装")
	}
	if m.goos() == "darwin" {
		if err := m.verifyBundle(ctx, staged.StagedPath); err != nil {
			return StagedUpdate{}, err
		}
	}
	exe, err := m.bundleExecutable(ctx, staged.StagedPath)
	if err != nil {
		return StagedUpdate{}, err
	}
	args := []string{
		"apply-update",
		"--staged", staged.StagedPath,
		"--target", staged.TargetPath,
	}
	if dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	if desktopPID > 0 {
		args = append(args, "--wait-pid", strconv.Itoa(desktopPID), "--wait-timeout", "20s")
	}
	if m.opts.LaunchRunner != nil {
		if err := m.opts.LaunchRunner(ctx, exe, args...); err != nil {
			return StagedUpdate{}, err
		}
		return staged, nil
	}

	logPath := filepath.Join(os.TempDir(), "vantry-update-apply.log")
	if dataDir != "" {
		_ = os.MkdirAll(dataDir, 0o700)
		logPath = filepath.Join(dataDir, "update-apply.log")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return StagedUpdate{}, fmt.Errorf("无法创建更新日志: %w", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	startErr := cmd.Start()
	_ = logFile.Close()
	if startErr != nil {
		return StagedUpdate{}, fmt.Errorf("无法启动后台更新程序: %w", startErr)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return staged, nil
}

func (m *Manager) bundleExecutable(ctx context.Context, bundle string) (string, error) {
	plistPath := filepath.Join(bundle, "Contents", "Info.plist")
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return "", fmt.Errorf("无法读取暂存应用 Info.plist: %w", err)
	}
	name := parsePlistExecutableXML(raw)
	if name == "" && runtime.GOOS == "darwin" {
		out, pbErr := exec.CommandContext(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :CFBundleExecutable", plistPath).Output()
		if pbErr == nil {
			name = strings.TrimSpace(string(out))
		}
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", errors.New("暂存应用缺少有效的 CFBundleExecutable")
	}
	exe := filepath.Join(bundle, "Contents", "MacOS", name)
	info, err := os.Stat(exe)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("暂存应用缺少可执行主程序")
	}
	return exe, nil
}

func parsePlistExecutableXML(raw []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	expectValue := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local == "key" {
			var key string
			if err := dec.DecodeElement(&key, &start); err != nil {
				return ""
			}
			expectValue = key == "CFBundleExecutable"
			continue
		}
		if expectValue {
			if start.Name.Local == "string" {
				var value string
				if err := dec.DecodeElement(&value, &start); err != nil {
					return ""
				}
				return strings.TrimSpace(value)
			}
			expectValue = false
		}
	}
}

func offlineCoreRequest(ctx context.Context, dataDir, method, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dataDir, "core.sock"))
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return errors.New("本机 Core 不可连接")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Core 拒绝退出，可能仍有任务、排队输入或审批；未替换应用")
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}
func offlineCoreStatus(ctx context.Context, dataDir string) (api.ServerStatus, error) {
	var status api.ServerStatus
	err := offlineCoreRequest(ctx, dataDir, http.MethodGet, "/v1/status", &status)
	return status, err
}
func drainOfflineCore(ctx context.Context, dataDir string) error {
	return offlineCoreRequest(ctx, dataDir, http.MethodPost, "/v1/admin/shutdown-if-idle", nil)
}
