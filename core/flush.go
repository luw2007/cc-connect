package core

import (
	"context"
	"sync"
	"time"
)

// FlushRender renders and pushes one card frame. It runs on the controller goroutine.
type FlushRender func(ctx context.Context) error

type FlushMetrics struct {
	Scheduled, Coalesced, Flushes, Failures                   int64
	TerminalDrains, TerminalDrainTimeouts, LastDrainLatencyMs int64
}

type FlushTask struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func newFlushTask() *FlushTask        { return &FlushTask{done: make(chan struct{})} }
func completedFlushTask() *FlushTask  { t := newFlushTask(); close(t.done); return t }
func (t *FlushTask) setErr(err error) { t.mu.Lock(); t.err = err; t.mu.Unlock() }
func (t *FlushTask) Err() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}
func (t *FlushTask) Done() <-chan struct{} { return t.done }
func (t *FlushTask) Wait(ctx context.Context, timeout time.Duration) bool {
	if t == nil {
		return true
	}
	if timeout <= 0 {
		select {
		case <-t.done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

type FlushController struct {
	mu                               sync.Mutex
	interval                         time.Duration
	latest                           FlushRender
	pending, pendingTerminal, closed bool
	lastFlushAt                      time.Time
	active                           *FlushTask
	wake                             chan struct{}
	metrics                          FlushMetrics
	now                              func() time.Time
}

func NewFlushController(interval time.Duration) *FlushController {
	if interval < 0 {
		interval = 0
	}
	return &FlushController{interval: interval, wake: make(chan struct{}, 1), now: time.Now}
}
func (fc *FlushController) Schedule(ctx context.Context, render FlushRender, terminal bool) *FlushTask {
	if fc == nil || render == nil {
		return completedFlushTask()
	}
	fc.mu.Lock()
	if fc.closed && !terminal {
		active := fc.active
		fc.mu.Unlock()
		if active != nil {
			return active
		}
		return completedFlushTask()
	}
	fc.metrics.Scheduled++
	fc.latest = render
	if fc.active != nil {
		fc.pending = true
		fc.pendingTerminal = fc.pendingTerminal || terminal
		fc.metrics.Coalesced++
		active := fc.active
		fc.mu.Unlock()
		if terminal {
			fc.signalWake()
		}
		return active
	}
	fc.pending = false
	fc.pendingTerminal = terminal
	task := newFlushTask()
	fc.active = task
	fc.mu.Unlock()
	go fc.run(ctx, task)
	return task
}
func (fc *FlushController) Drain(ctx context.Context, timeout time.Duration) bool {
	if fc == nil {
		return true
	}
	started := time.Now()
	fc.mu.Lock()
	fc.metrics.TerminalDrains++
	task := fc.active
	fc.mu.Unlock()
	if task == nil {
		fc.recordDrain(started, false)
		return true
	}
	ok := task.Wait(ctx, timeout)
	fc.recordDrain(started, !ok)
	return ok
}
func (fc *FlushController) Close() {
	if fc == nil {
		return
	}
	fc.mu.Lock()
	fc.closed = true
	fc.mu.Unlock()
}
func (fc *FlushController) Metrics() FlushMetrics {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.metrics
}
func (fc *FlushController) signalWake() {
	select {
	case fc.wake <- struct{}{}:
	default:
	}
}
func (fc *FlushController) recordDrain(start time.Time, timeout bool) {
	fc.mu.Lock()
	if timeout {
		fc.metrics.TerminalDrainTimeouts++
	}
	fc.metrics.LastDrainLatencyMs = time.Since(start).Milliseconds()
	fc.mu.Unlock()
}
func (fc *FlushController) clearActiveLocked(t *FlushTask) {
	if fc.active == t {
		fc.active = nil
	}
}
func (fc *FlushController) run(ctx context.Context, task *FlushTask) {
	for {
		fc.mu.Lock()
		terminal, last, interval := fc.pendingTerminal, fc.lastFlushAt, fc.interval
		fc.mu.Unlock()
		if !terminal && interval > 0 && !last.IsZero() {
			if delay := interval - fc.now().Sub(last); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					fc.mu.Lock()
					fc.clearActiveLocked(task)
					fc.mu.Unlock()
					task.setErr(ctx.Err())
					close(task.done)
					return
				case <-fc.wake:
					timer.Stop()
				case <-timer.C:
				}
			}
		}
		fc.mu.Lock()
		render := fc.latest
		terminal = fc.pendingTerminal
		if render == nil {
			fc.clearActiveLocked(task)
			fc.mu.Unlock()
			task.setErr(nil)
			close(task.done)
			return
		}
		fc.pending = false
		fc.pendingTerminal = false
		fc.metrics.Flushes++
		fc.mu.Unlock()
		err := render(ctx)
		fc.mu.Lock()
		fc.lastFlushAt = fc.now()
		if err != nil {
			fc.metrics.Failures++
		}
		task.setErr(err)
		if terminal || !fc.pending || ctx.Err() != nil {
			fc.clearActiveLocked(task)
			fc.mu.Unlock()
			close(task.done)
			return
		}
		fc.mu.Unlock()
	}
}
