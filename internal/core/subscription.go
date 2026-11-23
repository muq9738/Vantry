package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// subHub tracks live subscribers and the highest sequence published so far.
//
// The publish path never blocks: each subscriber gets a bounded live buffer,
// and a subscriber that cannot keep up is failed with resync_required and
// disconnected instead of stalling commits.
type subHub struct {
	core *Core
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	// lastSeq is the highest sequence handed to the hub. It is only informational
	// (snapshots read the log), but it makes commit/publish ordering observable.
	lastSeq int64
	closed  bool
}

func newSubHub(c *Core) *subHub {
	return &subHub{core: c, subs: make(map[*subscriber]struct{})}
}

func (h *subHub) publish(ev api.Event) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if ev.Sequence > h.lastSeq {
		h.lastSeq = ev.Sequence
	}
	var overflow []*subscriber
	for s := range h.subs {
		select {
		case s.live <- ev:
		default:
			overflow = append(overflow, s)
		}
	}
	h.mu.Unlock()

	for _, s := range overflow {
		s.fail(errorf(api.ResyncRequired, "subscriber buffer overflowed at sequence %d; resync from a snapshot", ev.Sequence))
	}
}

func (h *subHub) remove(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *subHub) closeAll() {
	h.mu.Lock()
	h.closed = true
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}

func (h *subHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Subscribe implements api.Service.Subscribe with the documented handshake:
// the live channel is registered first, the durable log is replayed from the
// cursor, and live events already covered by the replay are dropped by
// sequence. There is no window in which a committed event is delivered neither
// by the replay nor by the live path.
func (c *Core) Subscribe(ctx context.Context, after int64) (*api.Subscription, error) {
	if after < 0 {
		return nil, errorf(api.InvalidCommand, "after must not be negative")
	}
	if state := c.state.Load(); state >= stateDraining {
		return nil, errorf(api.NotReady, "core is %s", stateName(state))
	}
	size := c.subBufferSize
	if size <= 0 {
		size = defaultSubscriberBuffer
	}
	s := &subscriber{
		core:     c,
		hub:      c.hub,
		ctx:      ctx,
		after:    after,
		lastSent: after,
		live:     make(chan api.Event, size),
		out:      make(chan api.Event),
		errs:     make(chan error, 2),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	if err := c.hub.register(s); err != nil {
		return nil, err
	}
	go s.run()
	return &api.Subscription{Events: s.out, Errors: s.errs, Close: s.Close}, nil
}

func (h *subHub) register(s *subscriber) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || s.core.state.Load() >= stateDraining {
		return errorf(api.NotReady, "core is shutting down")
	}
	h.subs[s] = struct{}{}
	return nil
}

// subscriber owns one subscription's delivery goroutine. The goroutine is the
// only sender on out/errs and the only closer of them.
type subscriber struct {
	core  *Core
	hub   *subHub
	ctx   context.Context
	after int64

	live   chan api.Event
	out    chan api.Event
	errs   chan error
	done   chan struct{}
	exited chan struct{}

	doneOnce    sync.Once
	errOnce     sync.Once
	errMu       sync.Mutex
	terminalErr error

	// lastSent is owned by the delivery goroutine.
	lastSent int64
}

func (s *subscriber) run() {
	defer close(s.exited)
	defer func() {
		s.hub.remove(s)
		// Only this delivery goroutine sends on or closes the public channels.
		// A publisher holding an old pointer can still call fail after removal.
		s.errMu.Lock()
		err := s.terminalErr
		s.errMu.Unlock()
		if err != nil {
			s.errs <- err // buffered, with a single terminal sender
		}
		close(s.out)
		close(s.errs)
	}()

	if !s.replay() {
		return
	}
	for {
		select {
		case <-s.done:
			return
		case <-s.ctx.Done():
			return
		case ev := <-s.live:
			if ev.Sequence <= s.lastSent {
				continue
			}
			if !s.emit(ev) {
				return
			}
		}
	}
}

// replay pages through the durable log from the cursor. The page size bounds a
// single read; it is not a limit on how much history a subscriber may ask for.
func (s *subscriber) replay() bool {
	cursor := s.after
	for {
		events, err := s.core.loadEventsAfter(s.ctx, cursor, eventReplayPageSize)
		if err != nil {
			if s.stopping() {
				return false
			}
			if api.CodeOf(err) != api.ResyncRequired {
				err = wrapInternal("replay events", err)
			}
			s.fail(err)
			return false
		}
		for _, ev := range events {
			if ev.Sequence <= s.lastSent {
				continue
			}
			if !s.emit(ev) {
				return false
			}
		}
		if len(events) < eventReplayPageSize {
			return true
		}
		cursor = events[len(events)-1].Sequence
	}
}

func (s *subscriber) emit(ev api.Event) bool {
	select {
	case s.out <- ev:
		s.lastSent = ev.Sequence
		return true
	case <-s.done:
		return false
	case <-s.ctx.Done():
		return false
	}
}

func (s *subscriber) stopping() bool {
	select {
	case <-s.done:
		return true
	case <-s.ctx.Done():
		return true
	default:
		return false
	}
}

// fail delivers a terminal error (if any) and disconnects the subscriber.
func (s *subscriber) fail(err error) {
	if err != nil {
		s.errOnce.Do(func() {
			s.errMu.Lock()
			s.terminalErr = err
			s.errMu.Unlock()
		})
	}
	s.doneOnce.Do(func() { close(s.done) })
}

// Close is safe to call repeatedly and waits for the delivery goroutine to
// release its resources, so a caller can observe the release deterministically.
func (s *subscriber) Close() {
	s.doneOnce.Do(func() { close(s.done) })
	select {
	case <-s.exited:
	case <-time.After(subscriptionCloseTimeout):
	}
}

// loadEventsAfter checks retention and reads a page in one SQLite snapshot.
// Checking every page prevents concurrent pruning from silently skipping a gap.
func (c *Core) loadEventsAfter(ctx context.Context, cursor int64, limit int) ([]api.Event, error) {
	c.lifeMu.RLock()
	defer c.lifeMu.RUnlock()
	if c.state.Load() == stateStopped {
		return nil, errStoreClosing
	}
	tx, err := c.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var prunedThrough int64
	if err := tx.QueryRowContext(ctx, `SELECT int_value FROM core_metadata
		WHERE key = 'pruned_through_sequence'`).Scan(&prunedThrough); err != nil {
		return nil, err
	}
	if cursor > 0 && cursor < prunedThrough {
		return nil, errorf(api.ResyncRequired,
			"event cursor %d precedes pruned-through sequence %d; call Snapshot() and subscribe at its sequence", cursor, prunedThrough)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT sequence, id, type, thread_id, run_id, attempt_id, timestamp, data
		 FROM events WHERE sequence > ? ORDER BY sequence LIMIT ?`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []api.Event
	for rows.Next() {
		var (
			ev        api.Event
			timestamp int64
			data      []byte
		)
		if err := rows.Scan(&ev.Sequence, &ev.ID, &ev.Type, &ev.ThreadID, &ev.RunID,
			&ev.AttemptID, &timestamp, &data); err != nil {
			return nil, err
		}
		ev.Timestamp = fromNanos(timestamp)
		ev.Data = json.RawMessage(data)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
