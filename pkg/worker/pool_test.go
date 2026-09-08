package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolRunsTasks(t *testing.T) {
	var done atomic.Int32
	pool := New(Config{Workers: 4, QueueSize: 16})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		if !pool.Submit(func(context.Context) {
			defer wg.Done()
			done.Add(1)
		}) {
			wg.Done()
			t.Fatal("Submit was refused with room in the queue")
		}
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !pool.Shutdown(ctx) {
		t.Error("the pool did not drain")
	}
	if got := done.Load(); got != 10 {
		t.Errorf("ran %d tasks, want 10", got)
	}
}

func TestSubmitRefusesWhenTheQueueIsFull(t *testing.T) {
	// A full queue is real backpressure, and the caller has to be told: the
	// work it just failed to start has to be recorded somewhere. Blocking
	// instead would push the backlog into the minute tick.
	block := make(chan struct{})
	pool := New(Config{Workers: 1, QueueSize: 1})

	// One task occupies the worker, one fills the queue.
	pool.Submit(func(context.Context) { <-block })
	pool.Submit(func(context.Context) { <-block })

	refused := false
	for i := 0; i < 10; i++ {
		if !pool.Submit(func(context.Context) {}) {
			refused = true
			break
		}
	}
	close(block)

	if !refused {
		t.Error("Submit never reported a full queue")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Shutdown(ctx)
}

func TestTaskGetsItsOwnContext(t *testing.T) {
	// A task must not inherit the caller's context. A request context is
	// cancelled the moment the response is written, which would kill work that
	// had only just started.
	pool := New(Config{Workers: 1, QueueSize: 1, TaskTimeout: time.Minute})

	got := make(chan bool, 1)
	callerCtx, cancelCaller := context.WithCancel(context.Background())

	pool.Submit(func(ctx context.Context) {
		cancelCaller()
		// A moment for the cancellation to propagate, if it were going to.
		time.Sleep(20 * time.Millisecond)
		got <- ctx.Err() == nil
	})

	select {
	case alive := <-got:
		if !alive {
			t.Error("the task's context was cancelled with the caller's")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the task never ran")
	}
	_ = callerCtx

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Shutdown(ctx)
}

func TestTaskTimeoutCancelsTheTask(t *testing.T) {
	// A wedged target must not hold a worker forever.
	pool := New(Config{Workers: 1, QueueSize: 1, TaskTimeout: 50 * time.Millisecond})

	done := make(chan error, 1)
	pool.Submit(func(ctx context.Context) {
		<-ctx.Done()
		done <- ctx.Err()
	})

	select {
	case err := <-done:
		if err == nil {
			t.Error("the task context was not cancelled")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the task was never cancelled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Shutdown(ctx)
}

func TestPanicIsRecoveredAndReported(t *testing.T) {
	// Default recovery into stdout is invisible on a restarted container,
	// which is the exact moment it is needed.
	reported := make(chan string, 1)
	pool := New(Config{
		Workers: 1, QueueSize: 2,
		OnPanic: func(value any, stack string) { reported <- stack },
	})

	pool.Submit(func(context.Context) { panic("boom") })

	select {
	case stack := <-reported:
		if stack == "" {
			t.Error("no stack was reported with the panic")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the panic was not reported")
	}

	// The worker has to survive it and keep taking work.
	ran := make(chan struct{})
	pool.Submit(func(context.Context) { close(ran) })
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Error("the pool stopped working after a panic")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Shutdown(ctx)
}

func TestShutdownRefusesNewWorkAndIsIdempotent(t *testing.T) {
	pool := New(Config{Workers: 1, QueueSize: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !pool.Shutdown(ctx) {
		t.Fatal("the pool did not drain")
	}
	if pool.Submit(func(context.Context) {}) {
		t.Error("work was accepted after shutdown")
	}
	// A second call must not panic on a closed channel.
	pool.Shutdown(ctx)
}

func TestShutdownReportsAnIncompleteDrain(t *testing.T) {
	// A partial drain is worth reporting: tasks were abandoned, and the
	// operator should expect runs stuck in running until the watchdog closes
	// them.
	block := make(chan struct{})
	pool := New(Config{Workers: 1, QueueSize: 4})
	pool.Submit(func(context.Context) { <-block })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if pool.Shutdown(ctx) {
		t.Error("Shutdown reported a clean drain while a task was still running")
	}
	close(block)
}

func TestQueueDepthIsVisible(t *testing.T) {
	block := make(chan struct{})
	pool := New(Config{Workers: 1, QueueSize: 4})
	pool.Submit(func(context.Context) { <-block })
	pool.Submit(func(context.Context) {})

	if pool.Capacity() != 4 {
		t.Errorf("Capacity = %d, want 4", pool.Capacity())
	}
	if pool.Queued() < 1 {
		t.Errorf("Queued = %d, want at least the backlog of 1", pool.Queued())
	}

	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Shutdown(ctx)
}

func TestQueueSizeIsRaisedToTheWorkerCount(t *testing.T) {
	// A queue smaller than the worker count would refuse work the pool could
	// have started immediately.
	pool := New(Config{Workers: 8, QueueSize: 2})
	if pool.Capacity() < 8 {
		t.Errorf("Capacity = %d, want at least the 8 workers", pool.Capacity())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool.Shutdown(ctx)
}
