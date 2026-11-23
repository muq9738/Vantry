package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostprocess"
)

const (
	maxOutputBytes = 1 << 20
	maxOutputLines = 2000
	truncationNote = "[输出已截断：仅显示前 1 MiB / 2000 行；请在终端查看完整输出。]\n"
)

type limitedOutput struct {
	// Keep Buffer private: embedding it would also expose ReaderFrom, allowing
	// exec's io.Copy to bypass Write and allocate unbounded output.
	data      bytes.Buffer
	lines     int
	truncated bool
}

func (b *limitedOutput) Len() int      { return b.data.Len() }
func (b *limitedOutput) Bytes() []byte { return b.data.Bytes() }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutputBytes - b.Len()
	if b.truncated || remaining == 0 || b.lines == maxOutputLines {
		b.truncated = b.truncated || n > 0
		return n, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	for i, c := range p {
		if c == '\n' {
			b.lines++
			if b.lines == maxOutputLines {
				p = p[:i+1]
				break
			}
		}
	}
	_, _ = b.data.Write(p)
	b.truncated = len(p) < n
	return n, nil
}

func (b *limitedOutput) String() string {
	text := strings.ToValidUTF8(b.data.String(), "�")
	if b.truncated {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += truncationNote
	}
	return text
}

type commandResult struct{ stdout, stderr limitedOutput }

func (r *commandResult) text(stderrFirst bool) string {
	first, second := &r.stdout, &r.stderr
	if stderrFirst {
		first, second = second, first
	}
	var out limitedOutput
	_, _ = out.Write(first.Bytes())
	if first.Len() > 0 && second.Len() > 0 && !bytes.HasSuffix(first.Bytes(), []byte("\n")) {
		_, _ = out.Write([]byte("\n"))
	}
	_, _ = out.Write(second.Bytes())
	out.truncated = out.truncated || first.truncated || second.truncated
	return out.String()
}

func command(ctx context.Context, path string, args ...string) (string, error) {
	result, err := execute(ctx, path, args...)
	return result.stdout.String(), err
}

func execute(ctx context.Context, path string, args ...string) (commandResult, error) {
	return executeWithEnvironment(ctx, path, nil, args...)
}

// executeWithEnvironment adds operation-local variables after removing inherited
// repository routing. In particular checkpoints must never use the user index.
func executeWithEnvironment(ctx context.Context, path string, environment []string, args ...string) (commandResult, error) {
	return executeWithInput(ctx, path, environment, nil, args...)
}

func executeWithInput(ctx context.Context, path string, environment []string, input io.Reader, args ...string) (commandResult, error) {
	var out commandResult
	if err := ctx.Err(); err != nil {
		return out, err
	}
	cmd := hostprocess.NewContext(ctx, "git", append([]string{"-C", path}, args...)...)
	// Repository-routing variables inherited from Core's launcher must not
	// redirect a request to another index or checkout. Keep user configuration,
	// hooks, credential helpers, and signing settings intact.
	cmd.Env = append(workspace.GitEnvironment(), environment...)
	// Hooks and signing programs belong to the same cancellable operation.
	// Killing only Git would release the checkout lock while its hook still ran.
	defer cmd.Close()
	cmd.WaitDelay = time.Second
	cmd.Stdin = input
	cmd.Stdout, cmd.Stderr = &out.stdout, &out.stderr
	err := cmd.Run()
	if err != nil {
		if cmd.Process != nil && (ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay)) {
			_ = cmd.KillTree()
		}
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return out, fmt.Errorf("Git %s：%w\n%s", args[0], err, out.text(true))
	}
	return out, nil
}

func exitCode(err error, code int) bool {
	var exitErr *exec.ExitError
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		errors.As(err, &exitErr) && exitErr.ExitCode() == code
}
