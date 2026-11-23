package updater

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// RunApply is invoked from the downloaded bundle after (or while) the desktop
// exits to swap the verified app bundle and relaunch Vantry.
func RunApply(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("apply-update", flag.ContinueOnError)
	flags.SetOutput(output)
	staged := flags.String("staged", "", "已校验的更新 .app")
	target := flags.String("target", "", "原安装 .app")
	dataDir := flags.String("data-dir", "", "本机 Core 数据目录")
	waitPID := flags.Int("wait-pid", 0, "等待退出的桌面进程 PID")
	waitTimeout := flags.Duration("wait-timeout", 0, "等待桌面退出的最长耗时")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *staged == "" || *target == "" {
		return errors.New("apply-update 需要 --staged 和 --target")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("此安装命令仅适用于 macOS")
	}
	timeout := *waitTimeout
	if *waitPID > 0 && timeout <= 0 {
		timeout = 20 * time.Second
	}
	manager := NewManager(Options{})
	if err := manager.ApplyWithOptions(ctx, *staged, *target, *dataDir, ApplyOptions{
		WaitPID:     *waitPID,
		WaitTimeout: timeout,
	}); err != nil {
		return err
	}
	fmt.Fprintln(output, "更新已在旧应用和 Core 退出后安装，正在重新打开 Vantry。")
	// Remove only the download directory generated for this staged bundle.
	for p := filepath.Dir(*staged); p != filepath.Dir(p); p = filepath.Dir(p) {
		if strings.HasPrefix(filepath.Base(p), ".vantry-update-") && within(p, *staged) && !within(p, *target) {
			_ = os.RemoveAll(p)
			break
		}
	}
	if err := exec.CommandContext(ctx, "/usr/bin/open", *target).Run(); err != nil {
		if fallbackErr := exec.CommandContext(ctx, "/usr/bin/open", "-na", *target).Run(); fallbackErr != nil {
			return fmt.Errorf("更新已安装，请手动打开 Vantry: %w", fallbackErr)
		}
	}
	return nil
}
