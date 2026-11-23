package pi

import (
	"context"
	"io"
	"time"
)

type rpcWriteRequest struct {
	command  any
	complete func(error) error // called only by runRPC's state loop
}

type rpcWriteResult struct {
	request rpcWriteRequest
	err     error
}

// One writer owns stdin. runRPC keeps draining stdout while a large command
// waits for pipe space, including providers that print before reading input.
func startRPCWriter(ctx context.Context, stdin io.Writer) (chan<- rpcWriteRequest, <-chan rpcWriteResult, <-chan struct{}) {
	requests := make(chan rpcWriteRequest, 1)
	results := make(chan rpcWriteResult, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-requests:
				if pipe, ok := stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
					deadline := time.Now().Add(rpcTimeout)
					if caller, exists := ctx.Deadline(); exists && caller.Before(deadline) {
						deadline = caller
					}
					_ = pipe.SetWriteDeadline(deadline)
				}
				err := writeRPC(stdin, request.command)
				if pipe, ok := stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
					_ = pipe.SetWriteDeadline(time.Time{})
				}
				// At most the handshake pair or one control is in flight. The result
				// buffer always has room, including during cancellation/EOF finalization.
				results <- rpcWriteResult{request, err}
				if err != nil {
					return
				}
			}
		}
	}()
	return requests, results, done
}
