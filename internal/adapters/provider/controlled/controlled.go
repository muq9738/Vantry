// Package controlled provides a deterministic local provider for exercising the
// application without a model account or changes to the user's workspace.
package controlled

import (
	"context"
	"fmt"
	"time"

	"github.com/muq9738/Vantry/pkg/provider"
)

type Provider struct {
	Delay time.Duration
}

func (p Provider) Run(ctx context.Context, request provider.Request, emit func(string) error) error {
	text := []rune(fmt.Sprintf("这是 Vantry 的本机受控 Provider。\n\n已收到：%s\n\n任务已完成。", request.Message))
	for start := 0; start < len(text); start += 12 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.Delay > 0 {
			timer := time.NewTimer(p.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		end := min(start+12, len(text))
		if err := emit(string(text[start:end])); err != nil {
			return err
		}
	}
	return nil
}

var _ provider.Provider = Provider{}
