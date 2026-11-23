// Package sshrelay forwards Vantry's private socket through the user's SSH
// configuration. It does not expose an unauthenticated TCP listener.
package sshrelay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/pkg/api"
)

type Relay struct {
	*local.Client
	cancel    context.CancelFunc
	done      chan struct{}
	directory string
	closeErr  error // written before done closes

	mu        sync.Mutex
	lastError error
	stderr    diagnosticTail
}

func Validate(host, socket string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n\x00") {
		return errors.New("请输入 SSH 配置中的主机名或 user@host")
	}
	if !filepath.IsAbs(socket) || strings.ContainsAny(socket, ":\r\n\x00") {
		return errors.New("远程 socket 必须是绝对路径，且不能含冒号或换行")
	}
	return nil
}

// Connect waits for a ready Core through an SSH forward. ctx owns the entire
// relay lifetime, not just startup. Canceling it or calling Close disconnects
// this client only; no remote shell command or Core stop request is sent.
func Connect(ctx context.Context, host, socket string) (*Relay, error) {
	if ctx == nil {
		return nil, errors.New("sshrelay: nil context")
	}
	if err := Validate(host, socket); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "vantry-relay-")
	if err != nil {
		return nil, fmt.Errorf("创建 SSH 转发目录：%w", err)
	}
	relayCtx, cancel := context.WithCancel(ctx)
	localSocket := filepath.Join(directory, "core.sock")
	r := &Relay{Client: local.Dial(localSocket), cancel: cancel, done: make(chan struct{}), directory: directory}
	failures := make(chan error, 1)
	go r.forward(relayCtx, host, socket, localSocket, failures)
	ready, cancelReady := context.WithTimeout(ctx, 12*time.Second)
	defer cancelReady()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var healthError error
	fail := func(cause error) (*Relay, error) {
		closeErr := r.Close()
		return nil, errors.Join(fmt.Errorf("无法连接远程 Core：%w", cause), closeErr)
	}
	for {
		probe, cancelProbe := context.WithTimeout(ready, 300*time.Millisecond)
		health, err := r.Health(probe)
		cancelProbe()
		if err == nil {
			if health.Version != api.ProtocolVersion {
				return fail(&api.Error{Code: api.VersionMismatch, Message: fmt.Sprintf("远程 Core 协议为 %d，本客户端需要 %d", health.Version, api.ProtocolVersion)})
			}
			if health.State == "ready" && ready.Err() == nil {
				select {
				case err := <-failures:
					return fail(err)
				default:
					return r, nil
				}
			}
			healthError = fmt.Errorf("远程 Core 状态为 %q", health.State)
		} else if ready.Err() == nil || healthError == nil {
			healthError = err
		}
		select {
		case err := <-failures:
			return fail(err)
		case <-ready.Done():
			// A live ssh can still fail to open the remote socket. Keep that
			// stderr along with the HTTP failure instead of reporting only time.
			detail := r.stderr.String()
			return fail(fmt.Errorf("检查 SSH 主机信任、登录与 socket 路径（最近探测：%v；SSH：%s）：%w", healthError, detail, ready.Err()))
		case <-ticker.C:
		}
	}
}

func (r *Relay) forward(ctx context.Context, host, remoteSocket, localSocket string, failures chan<- error) {
	defer func() {
		_ = r.Client.Close()
		if err := os.RemoveAll(r.directory); err != nil {
			r.closeErr = fmt.Errorf("清理 SSH 转发目录：%w", err)
		}
		close(r.done)
	}()
	for ctx.Err() == nil {
		// Own one foreground ssh process even when the user's SSH config
		// enables multiplexing or ForkAfterAuthentication. Killing this child
		// must neither leave a background forward nor affect other sessions.
		command := exec.CommandContext(ctx, "ssh", "-N", "-T", "-S", "none",
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
			"-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes",
			"-o", "ForkAfterAuthentication=no", "-o", "ConnectTimeout=10",
			"-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
			"-L", localSocket+":"+remoteSocket, "--", host)
		r.stderr.Reset()
		command.Stderr = &r.stderr
		command.WaitDelay = time.Second
		err := command.Start()
		if err != nil {
			err = fmt.Errorf("启动 SSH：%w", err)
		} else if err = command.Wait(); err != nil {
			err = fmt.Errorf("SSH 转发退出：%w", err)
		} else {
			err = errors.New("SSH 转发意外退出（退出码 0）")
		}
		if ctx.Err() != nil {
			return
		}
		if detail := r.stderr.String(); detail != "" {
			err = fmt.Errorf("%w；SSH：%s", err, detail)
		}
		r.mu.Lock()
		r.lastError = err
		r.mu.Unlock()
		select {
		case failures <- err:
		default:
		}
		// SSH may leave its socket pathname after a crash. It is inside our
		// private directory; remove it before trying the next foreground child.
		if err := os.Remove(localSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.mu.Lock()
			r.lastError = errors.Join(r.lastError, fmt.Errorf("清理断开的转发 socket：%w", err))
			r.mu.Unlock()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// LastError returns the last unexpected SSH start/exit failure, including a
// bounded stderr tail. It stays available after reconnection; use Health to
// determine current reachability. Request/stream errors still use api.Service.
func (r *Relay) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastError
}

// Done closes after the SSH child is reaped and the local socket is removed.
func (r *Relay) Done() <-chan struct{} { return r.done }

func (r *Relay) Close() error {
	r.cancel()
	_ = r.Client.Close()
	<-r.done
	return r.closeErr
}

// diagnosticTail bounds memory even if a failing SSH/proxy writes continuously.
// Control characters are removed before diagnostics reach a UI or terminal.
type diagnosticTail struct {
	mu   sync.Mutex
	data []byte
}

const diagnosticLimit = 4 << 10

func (b *diagnosticTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= diagnosticLimit {
		b.data = append(b.data[:0], p[n-diagnosticLimit:]...)
	} else {
		if keep := diagnosticLimit - n; len(b.data) > keep {
			b.data = append(b.data[:0], b.data[len(b.data)-keep:]...)
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *diagnosticTail) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = b.data[:0]
}

func (b *diagnosticTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(string(b.data), "")))
}
