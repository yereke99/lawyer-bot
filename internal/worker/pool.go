package worker

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrQueueFull is returned when the pool cannot accept more work.
var ErrQueueFull = errors.New("worker queue is full")

// Job is a unit of background work. It receives a fresh context that is
// independent of the HTTP request that produced it.
type Job func(ctx context.Context)

// Pool is a bounded worker pool.
//
// The webhook handler must return quickly, but OpenAI calls take seconds. A
// bounded pool decouples the two without spawning an unbounded number of
// goroutines: when the queue is full, work is rejected loudly rather than
// silently piling up.
type Pool struct {
	jobs       chan Job
	workers    int
	jobTimeout time.Duration
	log        *zap.Logger

	wg      sync.WaitGroup
	started bool
	mu      sync.Mutex
	closed  bool
}

// Options configures the pool.
type Options struct {
	Workers    int
	QueueSize  int
	JobTimeout time.Duration
	Logger     *zap.Logger
}

// New builds a Pool.
func New(opts Options) *Pool {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.QueueSize < 1 {
		opts.QueueSize = 1
	}
	if opts.JobTimeout <= 0 {
		opts.JobTimeout = 60 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	return &Pool{
		jobs:       make(chan Job, opts.QueueSize),
		workers:    opts.Workers,
		jobTimeout: opts.JobTimeout,
		log:        opts.Logger,
	}
}

// Start launches the workers. The base context bounds the lifetime of every job.
func (p *Pool) Start(base context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true

	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.run(base, i)
	}
	p.log.Info("worker pool started",
		zap.Int("workers", p.workers),
		zap.Int("queue_size", cap(p.jobs)))
}

func (p *Pool) run(base context.Context, id int) {
	defer p.wg.Done()
	log := p.log.With(zap.Int("worker", id))

	for job := range p.jobs {
		func() {
			// A panic in one message must never take the bot down.
			defer func() {
				if r := recover(); r != nil {
					log.Error("worker recovered from panic", zap.Any("panic", r))
				}
			}()

			ctx, cancel := context.WithTimeout(base, p.jobTimeout)
			defer cancel()
			job(ctx)
		}()
	}
}

// Submit queues a job. It never blocks: if the queue is full the caller is told
// immediately, so the webhook can still return a fast response.
func (p *Pool) Submit(job Job) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errors.New("worker pool is closed")
	}

	select {
	case p.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// Shutdown stops accepting work and waits for in-flight jobs, bounded by ctx.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.jobs)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.log.Info("worker pool stopped")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pending reports the number of queued jobs, for health and metrics endpoints.
func (p *Pool) Pending() int {
	return len(p.jobs)
}
