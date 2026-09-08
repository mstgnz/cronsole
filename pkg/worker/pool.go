// Package worker is a bounded pool for background work.
//
// Four properties, and each one is here because its absence is a specific
// production failure:
//
//  1. Bounded concurrency AND a bounded queue. A raw `go func()` per task lets
//     a slow target turn a busy minute into thousands of goroutines.
//  2. Every task runs on its own context, never the caller's. A request
//     context is cancelled the moment the response is written, which would
//     kill work that had only just started.
//  3. A panic handler. Default recovery into stdout is invisible on a
//     restarted container, which is the exact moment it is needed.
//  4. A graceful drain. Work already accepted finishes before the process
//     exits, otherwise a shutdown loses runs that were reported as started.
package worker

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// Task is a unit of background work. It receives a context carrying its own
// timeout, unrelated to whatever asked for it.
type Task func(ctx context.Context)

// Pool runs tasks on a fixed set of workers behind a bounded queue.
type Pool struct {
	queue chan Task
	wg    sync.WaitGroup

	// taskTimeout bounds a single task. A task that outlives it is cancelled
	// through its context, so a wedged target cannot hold a worker forever.
	taskTimeout time.Duration

	// onPanic is called with the recovered value and the stack. It must not
	// panic itself.
	onPanic func(value any, stack string)

	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
}

// Config are the pool's settings.
type Config struct {
	Workers     int
	QueueSize   int
	TaskTimeout time.Duration
	OnPanic     func(value any, stack string)
}

// New starts a pool and its workers.
func New(cfg Config) *Pool {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.QueueSize < cfg.Workers {
		cfg.QueueSize = cfg.Workers
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 15 * time.Minute
	}

	p := &Pool{
		queue:       make(chan Task, cfg.QueueSize),
		taskTimeout: cfg.TaskTimeout,
		onPanic:     cfg.OnPanic,
	}
	for i := 0; i < cfg.Workers; i++ {
		p.wg.Add(1)
		go p.work()
	}
	return p
}

func (p *Pool) work() {
	defer p.wg.Done()
	for task := range p.queue {
		p.runOne(task)
	}
}

func (p *Pool) runOne(task Task) {
	defer func() {
		if rec := recover(); rec != nil {
			if p.onPanic != nil {
				p.onPanic(rec, string(debug.Stack()))
			} else {
				fmt.Printf("worker: recovered panic: %v\n%s\n", rec, debug.Stack())
			}
		}
	}()

	// context.Background, not the submitter's: a background task must outlive
	// the request that queued it.
	ctx, cancel := context.WithTimeout(context.Background(), p.taskTimeout)
	defer cancel()
	task(ctx)
}

// Submit queues a task. It returns false when the pool is closed or the queue
// is full.
//
// A false return is a real backpressure signal, not an ordinary outcome: the
// caller has to record the work it just failed to start. Blocking instead
// would push the backlog into whatever called Submit, which for the dispatcher
// means the minute tick.
func (p *Pool) Submit(task Task) bool {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	select {
	case p.queue <- task:
		return true
	default:
		return false
	}
}

// Queued is the current backlog depth, for metrics and the dashboard.
func (p *Pool) Queued() int { return len(p.queue) }

// Capacity is the queue size.
func (p *Pool) Capacity() int { return cap(p.queue) }

// Shutdown stops accepting work and waits for what is already queued, bounded
// by ctx.
//
// It returns whether the drain completed. A partial drain is worth reporting:
// it means tasks were abandoned and the operator should expect runs stuck in
// running until the watchdog closes them.
func (p *Pool) Shutdown(ctx context.Context) bool {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.queue)
	})

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
