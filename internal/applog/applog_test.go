package applog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- a driver that records what was written --------------------------------
//
// A real database is not needed to test the thing this package is actually
// about, which is the shape of the writes: one drainer, a bounded queue, and
// what happens when the far end is failing.

type recordingDriver struct {
	mu sync.Mutex
	// rows is every INSERT the drainer performed.
	rows []entry
	// err makes every write fail, standing in for a database outage.
	err error
	// block holds each write until it is closed, so the queue can be filled.
	block chan struct{}
	// written is signalled after every write attempt.
	written chan struct{}
}

func newRecordingDriver() *recordingDriver {
	return &recordingDriver{written: make(chan struct{}, 4096)}
}

func (d *recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{d: d}, nil }

func (d *recordingDriver) records() []entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]entry(nil), d.rows...)
}

func (d *recordingDriver) failWith(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

func (d *recordingDriver) blockWrites() {
	d.mu.Lock()
	d.block = make(chan struct{})
	d.mu.Unlock()
}

func (d *recordingDriver) releaseWrites() {
	d.mu.Lock()
	if d.block != nil {
		close(d.block)
		d.block = nil
	}
	d.mu.Unlock()
}

type recordingConn struct{ d *recordingDriver }

func (c *recordingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *recordingConn) Close() error                        { return nil }
func (c *recordingConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func (c *recordingConn) ExecContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	block, err := c.d.block, c.d.err
	c.d.mu.Unlock()

	if block != nil {
		<-block
	}
	if err == nil && len(args) >= 3 {
		c.d.mu.Lock()
		c.d.rows = append(c.d.rows, entry{
			level:   asString(args[0].Value),
			message: asString(args[1].Value),
			detail:  asString(args[2].Value),
		})
		c.d.mu.Unlock()
	}

	select {
	case c.d.written <- struct{}{}:
	default:
	}
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func asString(v driver.Value) string {
	s, _ := v.(string)
	return s
}

var driverSeq int

// attach opens a logger over the recording driver.
func attach(t *testing.T) (*Logger, *recordingDriver) {
	t.Helper()

	d := newRecordingDriver()
	driverSeq++
	name := "applog-test-" + itoa(driverSeq)
	sql.Register(name, d)

	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	l := New()
	l.Attach(db)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		l.Close(ctx)
	})
	return l, d
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// waitFor polls until cond holds, so a test never depends on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- what reaches the database ---------------------------------------------

func TestWarnAndErrorAreWrittenToTheDatabase(t *testing.T) {
	// The database copy exists because the operator is looking at a screen, and
	// because stdout is gone the moment a container restarts, which is exactly
	// when the last few lines mattered.
	l, d := attach(t)

	l.Warn("a warning", "some detail")
	l.Error("a failure", "why it failed")

	waitFor(t, "both entries to be written", func() bool { return len(d.records()) == 2 })

	rows := d.records()
	if rows[0].level != LevelWarn || rows[0].message != "a warning" || rows[0].detail != "some detail" {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].level != LevelError || rows[1].message != "a failure" {
		t.Errorf("second row = %+v", rows[1])
	}
}

func TestInfoIsNotWrittenToTheDatabase(t *testing.T) {
	// It is the highest volume level and the least useful to read back weeks
	// later. Writing it would make the table mostly noise.
	l, d := attach(t)

	for i := 0; i < 20; i++ {
		l.Info("routine", "n", i)
	}
	l.Warn("this one is written", "")
	waitFor(t, "the warning", func() bool { return len(d.records()) == 1 })

	if rows := d.records(); rows[0].message != "this one is written" {
		t.Errorf("an info entry reached the database: %+v", rows)
	}
}

func TestALoggerWithNoDatabaseIsUsableImmediately(t *testing.T) {
	// Start-up failures happen before the pool is open, and losing those is
	// losing the reason the process is about to exit.
	l := New()
	l.Warn("before any database", "detail")
	l.Error("also before", "detail")

	if got := l.Dropped(); got != 0 {
		t.Errorf("%d entries were dropped with an empty queue", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l.Close(ctx)
}

// --- the bounded queue ------------------------------------------------------

func TestTheQueueIsBoundedAndDropsRatherThanGrowing(t *testing.T) {
	// A goroutine per log call is the obvious shape and the wrong one: a
	// database outage produces thousands of errors a second, each spawning a
	// goroutine writing to the database that is down. The queue is capped, and
	// past it entries are dropped and the drop is itself reported.
	l, d := attach(t)

	// Hold the drainer inside its write so nothing leaves the queue.
	d.blockWrites()
	l.Error("the one being written", "")
	waitFor(t, "the drainer to be inside a write", func() bool {
		return len(l.queue) < queueSize
	})

	for i := 0; i < queueSize+200; i++ {
		l.Error("flood", "")
	}

	if got := l.Dropped(); got == 0 {
		t.Fatal("nothing was dropped, so the queue is not bounded")
	}
	if got := len(l.queue); got > queueSize {
		t.Errorf("the queue holds %d entries, over its cap of %d", got, queueSize)
	}

	d.releaseWrites()
}

func TestDroppedIsReportedSoAnIncompleteLogIsKnown(t *testing.T) {
	// Surfaced on the settings screen: a non-zero value means the log being
	// read is incomplete, and the reader has to know that rather than conclude
	// nothing happened.
	l, d := attach(t)

	d.blockWrites()
	l.Error("occupy the drainer", "")
	waitFor(t, "the drainer to be busy", func() bool { return len(l.queue) < queueSize })

	for i := 0; i < queueSize+50; i++ {
		l.Warn("flood", "")
	}
	first := l.Dropped()
	if first == 0 {
		t.Fatal("Dropped stayed at zero")
	}

	l.Warn("one more", "")
	if l.Dropped() <= first {
		t.Error("Dropped did not keep counting")
	}

	d.releaseWrites()
}

// --- failure of the far end -------------------------------------------------

func TestAFailedWriteDoesNotProduceAnotherLogEntry(t *testing.T) {
	// Reporting a failed log write by writing a log is how a database outage
	// becomes a loop. The failure goes to stdout only.
	l, d := attach(t)
	d.failWith(errors.New("connection refused"))

	before := len(d.records())
	for i := 0; i < 50; i++ {
		l.Error("this write will fail", "")
	}

	waitFor(t, "the failing writes to be attempted", func() bool {
		return len(d.written) >= 50
	})

	if got := len(d.records()); got != before {
		t.Errorf("%d rows were recorded despite every write failing", got-before)
	}
	// And the drainer is still alive: a write error must not end it, or the
	// first blip would silence the log for the life of the process.
	d.failWith(nil)
	l.Error("after the outage", "")
	waitFor(t, "logging to resume", func() bool {
		for _, row := range d.records() {
			if row.message == "after the outage" {
				return true
			}
		}
		return false
	})
}

// --- lifecycle --------------------------------------------------------------

func TestCloseDrainsTheBacklog(t *testing.T) {
	// Shutdown must not lose what is already queued: those entries are usually
	// the reason the process is going down.
	d := newRecordingDriver()
	driverSeq++
	name := "applog-drain-" + itoa(driverSeq)
	sql.Register(name, d)

	db, _ := sql.Open(name, "")
	defer func() { _ = db.Close() }()

	l := New()
	l.Attach(db)

	for i := 0; i < 100; i++ {
		l.Warn("queued before shutdown", "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.Close(ctx)

	if got := len(d.records()); got != 100 {
		t.Errorf("%d of 100 queued entries were written before shutdown finished", got)
	}
}

func TestCloseGivesUpWhenTheContextExpires(t *testing.T) {
	// A database that is not answering must not hold shutdown open forever.
	l, d := attach(t)

	d.blockWrites()
	for i := 0; i < 10; i++ {
		l.Error("stuck", "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	l.Close(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Close took %s, want it bounded by the context", elapsed)
	}

	d.releaseWrites()
}

func TestClosingALoggerThatWasNeverAttachedReturnsAtOnce(t *testing.T) {
	// Nothing drains it, so there is no backlog to wait for. Waiting anyway
	// blocks for the whole context, which during shutdown is the full timeout:
	// a process with no database configured would hang for ninety seconds on
	// its way out.
	l := New()
	l.Warn("before any database", "")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	done := make(chan struct{})
	go func() {
		l.Close(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a drainer that does not exist")
	}
}

func TestCloseTwiceDoesNotPanic(t *testing.T) {
	// Closing a closed channel panics, and shutdown paths get called twice by
	// a signal handler racing a deferred call.
	l, _ := attach(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l.Close(ctx)
	l.Close(ctx)
}

func TestAttachTwiceStartsOneDrainer(t *testing.T) {
	// Two drainers on one queue would write entries in an order neither of them
	// chose, and would double the load on a database already in trouble.
	d := newRecordingDriver()
	driverSeq++
	name := "applog-attach-" + itoa(driverSeq)
	sql.Register(name, d)

	db, _ := sql.Open(name, "")
	defer func() { _ = db.Close() }()

	l := New()
	l.Attach(db)
	l.Attach(db)
	l.Attach(db)

	for i := 0; i < 50; i++ {
		l.Warn("once each", "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.Close(ctx)

	if got := len(d.records()); got != 50 {
		t.Errorf("%d rows for 50 entries; more than one drainer is running", got)
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	// Every handler, the dispatcher and the worker pool log from their own
	// goroutines. Under -race this is the test that the queue and the drop
	// counter are actually protected.
	l, _ := attach(t)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Info("info", "j", j)
				l.Warn("warn", "detail")
				l.Error("error", "detail")
				_ = l.Dropped()
			}
		}()
	}
	wg.Wait()
}

func TestLoggingDuringShutdownDoesNotPanic(t *testing.T) {
	// A goroutine still finishing its work can log after Close has shut the
	// queue. Sending on a closed channel panics, and a panic in shutdown loses
	// everything else that was still draining.
	l, _ := attach(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l.Warn("still working", "")
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l.Close(ctx)

	close(stop)
	wg.Wait()
}
