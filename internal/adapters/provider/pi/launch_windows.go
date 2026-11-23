//go:build windows

package pi

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var npmNodeEntry = regexp.MustCompile(`(?i)"(?:%~dp0|%dp0%)\\([^"\r\n]+\.(?:c|m)?js)"`)

// npm's Windows bin entry is a batch shim, not a CreateProcess executable.
// Resolve its Node script and pass arguments directly, without cmd.exe parsing.
func piLaunchArguments(executable string, args []string) (string, []string, error) {
	ext := strings.ToLower(filepath.Ext(executable))
	entry := ""
	switch ext {
	case ".cmd", ".bat":
		source, err := os.ReadFile(executable)
		if err != nil {
			return "", nil, err
		}
		if len(source) > 128<<10 {
			return "", nil, errors.New("Pi 启动脚本过大")
		}
		match := npmNodeEntry.FindSubmatch(source)
		if len(match) != 2 || !strings.Contains(strings.ToLower(string(source)), "node") {
			return "", nil, errors.New("请为 Pi 配置可执行文件或 npm 的 Node.js CLI 入口")
		}
		entry = filepath.Clean(filepath.Join(filepath.Dir(executable), string(match[1])))
	case ".js", ".mjs", ".cjs":
		entry = executable
	default:
		return executable, args, nil
	}
	info, err := os.Stat(entry)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, errors.New("Pi npm 启动脚本对应的 CLI 文件不存在")
	}
	node := filepath.Join(filepath.Dir(executable), "node.exe")
	if info, err := os.Stat(node); err != nil || !info.Mode().IsRegular() {
		node, err = exec.LookPath("node.exe")
		if err != nil {
			return "", nil, errors.New("Pi 需要本机已有的 Node.js")
		}
	}
	return node, append([]string{entry}, args...), nil
}
