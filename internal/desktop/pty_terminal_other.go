//go:build !darwin && !linux

package desktop

import (
	"time"

	"github.com/egoist/mygo/plugins/terminal"
)

// programRun is the program of a terminal here, which Vantry's own pty does
// not provide outside macOS and Linux: the terminal library starts it, so
// there is nothing to watch and the methods answer for no program.
type programRun struct {
	code int
	done chan struct{}
}

func (r *programRun) Code() int                   { return r.code }
func (r *programRun) CodeAfter(time.Duration) int { return r.code }
func (r *programRun) Exited() <-chan struct{} {
	if r == nil || r.done == nil {
		return nil
	}
	return r.done
}
func (r *programRun) Tail() string { return "" }

// launchTerminal starts argv through the terminal library, whose Windows
// ConPTY has no such read problem as the pty of macOS.
func (w *workbench) launchTerminal(options terminal.Options, argv []string, final func(code int) string) (*terminal.Terminal, *programRun, error) {
	if w.newTerminal != nil {
		options.Command = argv
		term, err := w.newTerminal(options)
		return term, nil, err
	}
	options.Command = argv
	term, err := terminal.New(options)
	if err != nil {
		return nil, nil, err
	}
	return term, nil, nil
}
