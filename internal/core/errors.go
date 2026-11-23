package core

import (
	"errors"
	"time"
)

var (
	// errStoreClosing means the core is shutting down; the write was refused
	// before touching the database.
	errStoreClosing = errors.New("core: store is closing")
	// errStaleAttempt means a condition check inside a transaction failed: the
	// run, its active attempt, or the outbox claim no longer belongs to the
	// caller. The transaction rolls back without writing anything.
	errStaleAttempt     = errors.New("core: attempt is no longer current")
	errProviderEventGap = errors.New("core: provider event gap")
)

const (
	// defaultShutdownGrace bounds how long Close waits for providers that do not
	// answer cancellation.
	defaultShutdownGrace = 2 * time.Second
	// settleTimeout bounds the final drain write after the grace period.
	settleTimeout = 5 * time.Second
	// defaultSubscriberBuffer bounds each subscriber's live buffer. Overflow is
	// reported as resync_required instead of blocking the commit path.
	defaultSubscriberBuffer = 1024
	// eventReplayPageSize bounds a single read while replaying the log. It is a
	// page size, not a cap on how much history a subscriber may request.
	eventReplayPageSize = 256
	// workerIdleInterval is the fallback poll interval when no wake-up arrives.
	workerIdleInterval = 250 * time.Millisecond
	// subscriptionCloseTimeout guards Close against a wedged delivery goroutine.
	subscriptionCloseTimeout = 2 * time.Second
)
