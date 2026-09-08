// Package applog writes application level events to stdout and to the
// database.
//
// The database copy exists because the operator is looking at a screen, not a
// terminal, and because stdout is gone the moment a container restarts, which
// is precisely when the last few lines mattered.
//
// Writes go through a bounded channel drained by ONE goroutine. A goroutine
// per log call is the obvious shape and the wrong one: a database outage
// produces thousands of errors a second, each of which would then spawn a
// goroutine trying to write to the database that is down.
package applog

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// queueSize bounds the backlog. Past it, entries are dropped and the drop is
// itself reported to stdout: losing log lines is bad, and growing without
// bound during an outage is worse.
const queueSize = 1024

type entry struct {
	level   string
	message string
	detail  string
	at      time.Time
}

// Logger writes to stdout always, and to the database when one is attached.
type Logger struct {
	queue chan entry

	// mu guards db and closed. enqueue holds it for READING across its send,
	// which is what makes closing the queue safe: Close cannot take the write
	// lock until every send in flight has finished, and no send can start after
	// it has. Sending on a closed channel is a panic, and a panic in a
	// background goroutine during shutdown takes the process down with whatever
	// was still draining.
	mu     sync.RWMutex
	db     *sql.DB
	closed bool

	// dropped is atomic rather than under mu, so the drop path does not need to
	// upgrade a read lock to a write lock while holding it.
	dropped atomic.Int64

	// draining says whether a drainer goroutine exists. Close waits for the
	// backlog only when one does: nothing drains a logger that was never
	// attached to a database, so waiting would block for the whole context,
	// which during shutdown is the full timeout.
	draining atomic.Bool

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
}

// New builds a logger with no database attached. It is usable immediately, so
// failures during start up are not lost waiting for a connection.
func New() *Logger {
	return &Logger{
		queue: make(chan entry, queueSize),
		done:  make(chan struct{}),
	}
}

// Attach connects the logger to a database and starts the drainer. Calling it
// twice starts one drainer, which is why the guard is here rather than in the
// caller.
func (l *Logger) Attach(db *sql.DB) {
	l.mu.Lock()
	l.db = db
	l.mu.Unlock()

	l.startOnce.Do(func() {
		l.draining.Store(true)
		go l.drain()
	})
}

func (l *Logger) drain() {
	for e := range l.queue {
		l.mu.RLock()
		db := l.db
		l.mu.RUnlock()
		if db == nil {
			continue
		}

		// The write gets its own short context. It must not inherit anything
		// that is already being cancelled, and it must not be able to hang.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := db.ExecContext(ctx,
			`INSERT INTO app_logs (level, message, detail, created_at) VALUES ($1, $2, $3, $4)`,
			e.level, e.message, e.detail, e.at)
		cancel()
		if err != nil {
			// Deliberately stdout only. Reporting a failed log write by
			// writing a log is how a database outage becomes a loop.
			slog.Warn("applog: write failed", "err", err, "message", e.message)
		}
	}
	close(l.done)
}

// Close stops the drainer and waits for the backlog, bounded by ctx.
//
// The flag is set under the WRITE lock before the channel is closed, so no
// send can be in flight when it happens and none can start afterwards. Closing
// it without that is a panic in whichever goroutine logs next, and during
// shutdown there are several still running: the worker pool drains on its own
// deadline, and the watchdog and the samplers stop on theirs.
func (l *Logger) Close(ctx context.Context) {
	l.stopOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.queue)
		l.mu.Unlock()
	})
	if !l.draining.Load() {
		// Never attached, so there is no backlog and nothing to wait for.
		return
	}
	select {
	case <-l.done:
	case <-ctx.Done():
	}
}

func (l *Logger) enqueue(level, message, detail string) {
	// Held for reading across the send. See the note on Logger.mu.
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.closed {
		// Shutting down. The entry is already on stdout, which is where anything
		// logged this late is read from anyway; counting it keeps the tally
		// honest rather than reporting a complete log.
		l.dropped.Add(1)
		return
	}

	select {
	case l.queue <- entry{level: level, message: message, detail: detail, at: time.Now()}:
	default:
		// Deliberately stdout only, and never a database write: the queue being
		// full usually means the database is the thing that is struggling.
		slog.Warn("applog: queue full, entry dropped",
			"dropped_total", l.dropped.Add(1), "message", message)
	}
}

// Info records an ordinary event. Info is not written to the database: it is
// the highest volume level and the least useful one to read back weeks later.
func (l *Logger) Info(message string, args ...any) {
	slog.Info(message, args...)
}

// Warn records something that needs attention but did not stop the work.
func (l *Logger) Warn(message, detail string, args ...any) {
	slog.Warn(message, append(args, "detail", detail)...)
	l.enqueue(LevelWarn, message, detail)
}

// Error records a failure.
func (l *Logger) Error(message, detail string, args ...any) {
	slog.Error(message, append(args, "detail", detail)...)
	l.enqueue(LevelError, message, detail)
}

// Dropped is how many entries the queue has discarded. Surfaced on the
// settings screen, because a non-zero value means the log being read is
// incomplete and the reader has to know that.
func (l *Logger) Dropped() int64 { return l.dropped.Load() }
