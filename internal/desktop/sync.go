package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// resync is the one channel the view thread uses to tell the event worker to
// drop its subscription and take a fresh snapshot. It carries no state, so no
// goroutine other than the worker reads it.
type resync struct {
	ch chan string
}

func newResync() *resync { return &resync{ch: make(chan string, 1)} }

// ask requests a resync without ever blocking the caller: one request is enough
// to re-establish the stream, and extra reasons lose nothing.
func (r *resync) ask(reason string) {
	select {
	case r.ch <- reason:
	default:
	}
}

var (
	syncStreamInitialBackoff = 200 * time.Millisecond
	syncStreamMaxBackoff     = 5 * time.Second
	syncStreamStableWindow   = 10 * time.Second
	syncFrameCoalesceWindow  = 16 * time.Millisecond
)

const syncFrameMaxBatch = 64

func isCoalesceableStreamEvent(ev api.Event) bool {
	return ev.Type == eventRunOutput || ev.Type == eventRunReasoning
}

// syncWorker keeps the view in step with Core: snapshot first, then the events
// after that snapshot's cursor, and - after any gap, unknown event or stream
// failure - a new snapshot before subscribing again.
func (w *workbench) syncWorker(ctx context.Context, res *resync) {
	w.checkHealth(ctx)
	delay := 400 * time.Millisecond
	streamDelay := syncStreamInitialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		reason, explicit, err := w.syncOnceDetailed(ctx, res)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.post(func() { w.noteSyncFailure(err) })
			if !waitCtx(ctx, delay) {
				return
			}
			delay = min(syncStreamMaxBackoff, delay*2)
			continue
		}
		delay = 400 * time.Millisecond
		if reason != "" {
			w.post(func() { w.noteResync(reason) })
		}
		if explicit || time.Since(started) >= syncStreamStableWindow {
			streamDelay = syncStreamInitialBackoff
			continue
		}
		if !waitCtx(ctx, streamDelay) {
			return
		}
		streamDelay = min(syncStreamMaxBackoff, streamDelay*2)
	}
}

// syncOnce takes one snapshot, subscribes after its cursor and folds events in
// until the stream has to be rebuilt. reason says why it ended; a nil reason
// with a nil error means nothing more is pending.
func (w *workbench) syncOnce(ctx context.Context, res *resync) (reason string, err error) {
	reason, _, err = w.syncOnceDetailed(ctx, res)
	return reason, err
}

func (w *workbench) syncOnceDetailed(ctx context.Context, res *resync) (reason string, explicit bool, err error) {
	snap, err := w.service.Snapshot(ctx)
	if err != nil {
		return "", false, err
	}
	w.post(func() { w.applySnapshot(snap) })

	sub, err := w.service.Subscribe(ctx, snap.Sequence)
	if err != nil {
		return "", false, err
	}
	if sub == nil {
		return "", false, errors.New("Subscribe 返回空订阅")
	}
	defer sub.Close()

	w.post(func() { w.noteSubscribed(snap.Sequence) })
	reason, explicit, err = w.drainStreamDetailed(ctx, sub, res)
	return reason, explicit, err
}

// checkHealth records what Core says about itself, so the window can tell a
// transport that cannot be reached from a Core that is merely not ready.
func (w *workbench) checkHealth(ctx context.Context) {
	service := w.service
	w.start(func() {
		health, err := service.Health(ctx)
		w.post(func() {
			switch {
			case err != nil:
				w.healthNotice = notice{Text: describe("Core 健康检查", err), Level: levelFor(err)}
				w.connNotice = notice{Text: describe("Core 通信", err), Level: levelFor(err)}
			case health.Version != api.ProtocolVersion:
				w.healthNotice = notice{
					Text:  "本地服务版本与界面不兼容。",
					Level: noticeError,
				}
			default:
				w.healthNotice = notice{}
				w.connNotice = w.connectedNotice()
			}
		})
	})
}

// drainStream reads one subscription. Events are applied on the view thread;
// when the view cannot understand one, it asks for a resync through the channel
// instead of reaching into the worker's state.
func (w *workbench) drainStream(ctx context.Context, sub *api.Subscription, res *resync) (string, error) {
	reason, _, err := w.drainStreamDetailed(ctx, sub, res)
	return reason, err
}

func (w *workbench) drainStreamDetailed(ctx context.Context, sub *api.Subscription, res *resync) (string, bool, error) {
	var (
		events     = sub.Events
		errs       = sub.Errors
		pending    []api.Event
		flushTimer *time.Timer
		flushCh    <-chan time.Time
	)
	defer func() {
		if flushTimer != nil {
			flushTimer.Stop()
		}
	}()
	flushPending := func() {
		if flushTimer != nil {
			if !flushTimer.Stop() {
				select {
				case <-flushTimer.C:
				default:
				}
			}
		}
		flushCh = nil
		if len(pending) == 0 {
			return
		}
		batch := pending
		pending = nil
		w.post(func() {
			for _, ev := range batch {
				if !w.applyEvent(ev) {
					res.ask(fmt.Sprintf("收到无法折叠的事件 %q（游标 %d）", ev.Type, ev.Sequence))
					return
				}
			}
		})
	}
	for {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case reason := <-res.ch:
			flushPending()
			return reason, true, nil
		case <-flushCh:
			flushCh = nil
			flushPending()
		case ev, ok := <-events:
			if !ok {
				flushPending()
				if errs != nil {
					select {
					case err, errOK := <-errs:
						if errOK && err != nil {
							w.post(func() {
								w.noteStream(describe("事件流", err), levelFor(err))
							})
							return "事件流已断开", false, nil
						}
					default:
					}
				}
				return "事件流已关闭", false, nil
			}
			pending = append(pending, ev)
			if !isCoalesceableStreamEvent(ev) || syncFrameCoalesceWindow <= 0 || len(pending) >= syncFrameMaxBatch {
				flushPending()
			} else if flushCh == nil {
				if flushTimer == nil {
					flushTimer = time.NewTimer(syncFrameCoalesceWindow)
				} else {
					flushTimer.Reset(syncFrameCoalesceWindow)
				}
				flushCh = flushTimer.C
			}
		case err, ok := <-errs:
			if !ok {
				// Closed error channel: the stream reports failure by ending.
				errs = nil
				continue
			}
			flushPending()
			if err != nil {
				w.post(func() {
					w.noteStream(describe("事件流", err), levelFor(err))
				})
			}
			return "事件流已断开", false, nil
		}
	}
}

func (w *workbench) noteStream(text string, level noticeLevel) {
	w.streamNotice = notice{Text: text, Level: level}
}

// noteSyncFailure reports a failed sync round: the connection line says what is
// wrong with Core, the stream line says what this client is doing about it, and
// the reason stays so the recovery line can name it.
func (w *workbench) noteSyncFailure(err error) {
	text := describe("Core 同步", err)
	w.connNotice = notice{Text: text, Level: levelFor(err)}
	w.streamNotice = notice{Text: text + "；正在重新取快照并重连事件流", Level: noticeWarn}
	w.resyncReason = text
}

// noteResync remembers why the stream was rebuilt, so the line the operator
// reads after recovery still names the failure.
func (w *workbench) noteResync(reason string) {
	w.resyncReason = reason
	w.streamNotice = notice{Text: "连接已中断，正在恢复；已有内容会保留。", Level: noticeWarn}
}

func (w *workbench) noteSubscribed(sequence int64) {
	w.resyncReason = ""
	w.streamNotice = notice{}
}

func waitCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
