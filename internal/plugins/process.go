package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/internal/hostprocess"
)

const (
	processProtocolVersion = 1
	maxProcessStdoutBytes  = maxJSONBytes
	maxProcessStderrBytes  = 8 << 10
)

type processRequest struct {
	Version   int             `json:"version"`
	PluginID  string          `json:"pluginId"`
	Workspace string          `json:"workspace"`
	Input     json.RawMessage `json:"input"`
}

type resolvedProcessCommand struct {
	executable  string
	args        []string
	pluginDir   string
	entryRel    string
	requireExec bool
}

func resolveProcessCommand(pluginDir string, d definition) (resolvedProcessCommand, error) {
	if d.err != nil {
		return resolvedProcessCommand{}, d.err
	}
	if len(d.Command) == 0 {
		return resolvedProcessCommand{}, errors.New("process 插件必须配置非空的 command")
	}
	for i, arg := range d.Command {
		if strings.ContainsRune(arg, 0) {
			return resolvedProcessCommand{}, fmt.Errorf("process 插件 command[%d] 包含非法空字符", i)
		}
	}
	cmd0 := d.Command[0]
	if strings.TrimSpace(cmd0) == "" {
		return resolvedProcessCommand{}, errors.New("process 插件 command[0] 不能为空")
	}

	absPluginDir, err := filepath.Abs(pluginDir)
	if err != nil {
		return resolvedProcessCommand{}, fmt.Errorf("解析插件目录路径：%w", err)
	}
	root, err := os.OpenRoot(absPluginDir)
	if err != nil {
		return resolvedProcessCommand{}, fmt.Errorf("打开插件目录：%w", err)
	}
	defer root.Close()

	if d.Entry != "" {
		entryRel, ok := cleanPluginRelPath(d.Entry)
		if !ok {
			return resolvedProcessCommand{}, errors.New("process 插件 entry 必须在插件目录内")
		}
		info, err := lstatNoSymlink(root, entryRel)
		if err != nil {
			return resolvedProcessCommand{}, fmt.Errorf("校验插件入口 %q：%w", d.Entry, err)
		}
		if !info.Mode().IsRegular() {
			return resolvedProcessCommand{}, fmt.Errorf("插件入口 %q 必须是普通文件", d.Entry)
		}
	}

	// Case 1: direct executable path inside the plugin directory (e.g. ["./plugin-bin"]).
	if strings.ContainsRune(cmd0, '/') || strings.ContainsRune(cmd0, '\\') {
		cleanExe, ok := cleanPluginRelPath(cmd0)
		if !ok {
			return resolvedProcessCommand{}, fmt.Errorf("process 插件可执行文件 %q 必须在插件目录内", cmd0)
		}
		info, err := lstatNoSymlink(root, cleanExe)
		if err != nil {
			return resolvedProcessCommand{}, fmt.Errorf("读取可执行入口 %q：%w", cmd0, err)
		}
		if !info.Mode().IsRegular() {
			return resolvedProcessCommand{}, fmt.Errorf("可执行入口 %q 必须是普通文件（不支持符号链接）", cmd0)
		}
		if !hostfs.Executable(info) {
			return resolvedProcessCommand{}, fmt.Errorf("可执行入口 %q 缺少可执行权限", cmd0)
		}
		return resolvedProcessCommand{
			executable:  filepath.Join(absPluginDir, filepath.FromSlash(cleanExe)),
			args:        append([]string(nil), d.Command[1:]...),
			pluginDir:   absPluginDir,
			entryRel:    cleanExe,
			requireExec: true,
		}, nil
	}

	// Case 2: interpreter in PATH + script file inside the plugin directory (e.g. ["python3", "main.py"]).
	exePath, err := exec.LookPath(cmd0)
	if err != nil {
		return resolvedProcessCommand{}, fmt.Errorf("查找进程插件解释器 %q：%w", cmd0, err)
	}
	scriptArg, err := findProcessScriptArg(d.Command[1:])
	if err != nil {
		return resolvedProcessCommand{}, err
	}
	cleanScript, ok := cleanPluginRelPath(scriptArg)
	if !ok {
		return resolvedProcessCommand{}, fmt.Errorf("process 插件入口脚本 %q 必须在插件目录内", scriptArg)
	}
	info, err := lstatNoSymlink(root, cleanScript)
	if err != nil {
		return resolvedProcessCommand{}, fmt.Errorf("读取入口脚本 %q：%w", scriptArg, err)
	}
	if !info.Mode().IsRegular() {
		return resolvedProcessCommand{}, fmt.Errorf("入口脚本 %q 必须是普通文件（不支持符号链接）", scriptArg)
	}

	return resolvedProcessCommand{
		executable: exePath,
		args:       append([]string(nil), d.Command[1:]...),
		pluginDir:  absPluginDir,
		entryRel:   cleanScript,
	}, nil
}

func findProcessScriptArg(args []string) (string, error) {
	afterDoubleDash := false
	for _, arg := range args {
		if !afterDoubleDash {
			if arg == "--" {
				afterDoubleDash = true
				continue
			}
			if arg == "-c" || arg == "-e" || arg == "-m" || arg == "--eval" || strings.HasPrefix(arg, "--eval=") {
				return "", fmt.Errorf("process 插件不允许使用 %q 内联参数，必须指定插件目录内的入口脚本", arg)
			}
			if strings.HasPrefix(arg, "-") && arg != "-" {
				continue
			}
		}
		if arg == "" || arg == "-" {
			return "", errors.New("process 插件必须指定有效的目录内入口脚本")
		}
		return arg, nil
	}
	return "", errors.New("process 插件必须在 command 中指定目录内的入口脚本")
}

func runProcessPlugin(ctx context.Context, resolved resolvedProcessCommand, id, workspaceDir string, input json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", context.Cause(ctx)
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if len(input) > maxJSONBytes || !json.Valid(input) {
		return "", errors.New("插件输入必须是 1 MiB 以内的 JSON")
	}

	// Re-verify the entry inside os.Root right before spawning to prevent TOCTOU symlink swaps.
	if err := verifyProcessEntry(resolved); err != nil {
		return "", err
	}

	payload, err := json.Marshal(processRequest{
		Version:   processProtocolVersion,
		PluginID:  id,
		Workspace: workspaceDir,
		Input:     input,
	})
	if err != nil {
		return "", fmt.Errorf("编码进程插件请求：%w", err)
	}
	payload = append(payload, '\n')

	cmd := hostprocess.New(resolved.executable, resolved.args...)
	cmd.Dir = resolved.pluginDir
	defer cmd.Close()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("创建进程插件 stdin：%w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("创建进程插件 stdout：%w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("创建进程插件 stderr：%w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动进程插件：%w", err)
	}

	execCtx, stopExec := context.WithCancel(ctx)
	defer stopExec()

	killed := make(chan struct{})
	stopKill := context.AfterFunc(execCtx, func() {
		_ = cmd.KillTree()
		close(killed)
	})

	var (
		wg             sync.WaitGroup
		stdoutBytes    []byte
		stdoutOverflow bool
		stderrBytes    []byte
		stderrTrunc    bool
	)

	wg.Add(3)
	go func() {
		defer wg.Done()
		_, _ = stdinPipe.Write(payload)
		_ = stdinPipe.Close()
	}()
	go func() {
		defer wg.Done()
		stdoutBytes, stdoutOverflow = readBoundedStopOnOverflow(stdoutPipe, maxProcessStdoutBytes, stopExec)
	}()
	go func() {
		defer wg.Done()
		stderrBytes, stderrTrunc = readBoundedDrain(stderrPipe, maxProcessStderrBytes)
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	if !stopKill() {
		<-killed
	}
	cmd.EnsureGone()

	if err := ctx.Err(); err != nil {
		return "", context.Cause(ctx)
	}
	if stdoutOverflow {
		return "", errors.New("插件输出超过 1 MiB")
	}
	if waitErr != nil {
		diag := formatStderrDiagnostic(stderrBytes, stderrTrunc)
		if diag != "" {
			return "", fmt.Errorf("进程插件退出异常（%w）：%s", waitErr, diag)
		}
		return "", fmt.Errorf("进程插件退出异常：%w", waitErr)
	}

	output := string(stdoutBytes)
	output = strings.TrimSuffix(output, "\n")
	output = strings.TrimSuffix(output, "\r")
	if len(output) > maxProcessStdoutBytes {
		return "", errors.New("插件输出超过 1 MiB")
	}
	return output, nil
}

func verifyProcessEntry(resolved resolvedProcessCommand) error {
	root, err := os.OpenRoot(resolved.pluginDir)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := lstatNoSymlink(root, resolved.entryRel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("进程插件入口必须是普通文件（不支持符号链接）")
	}
	if resolved.requireExec && !hostfs.Executable(info) {
		return errors.New("进程插件可执行入口缺少可执行权限")
	}
	return nil
}

func readBoundedStopOnOverflow(r io.Reader, limit int, onOverflow func()) ([]byte, bool) {
	var out []byte
	buf := make([]byte, 16<<10)
	overflow := false
	for {
		n, err := r.Read(buf)
		if n > 0 && !overflow {
			remaining := limit - len(out)
			if n > remaining {
				out = append(out, buf[:remaining]...)
				overflow = true
				onOverflow()
			} else {
				out = append(out, buf[:n]...)
			}
		}
		if err != nil {
			break
		}
	}
	return out, overflow
}

func readBoundedDrain(r io.Reader, limit int) ([]byte, bool) {
	var out []byte
	buf := make([]byte, 4<<10)
	truncated := false
	for {
		n, err := r.Read(buf)
		if n > 0 && len(out) < limit {
			remaining := limit - len(out)
			if n > remaining {
				out = append(out, buf[:remaining]...)
				truncated = true
			} else {
				out = append(out, buf[:n]...)
			}
		} else if n > 0 {
			truncated = true
		}
		if err != nil {
			break
		}
	}
	return out, truncated
}

func formatStderrDiagnostic(raw []byte, truncated bool) string {
	for len(raw) > 0 && !utf8.Valid(raw) {
		raw = raw[:len(raw)-1]
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}
	if truncated {
		return text + " [stderr 已截断]"
	}
	return text
}
