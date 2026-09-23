package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlushController_CoalescesToLatest(t *testing.T) {
	fc := NewFlushController(50 * time.Millisecond)
	var mu sync.Mutex
	got := -1
	calls := 0
	var task *FlushTask
	for i := 0; i < 10; i++ {
		i := i
		task = fc.Schedule(context.Background(), func(context.Context) error { mu.Lock(); got = i; calls++; mu.Unlock(); return nil }, false)
	}
	if !task.Wait(context.Background(), time.Second) {
		t.Fatal("flush timed out")
	}
	mu.Lock()
	defer mu.Unlock()
	if got != 9 {
		t.Fatalf("latest=%d", got)
	}
	if calls > 3 {
		t.Fatalf("calls=%d", calls)
	}
	if fc.Metrics().Coalesced == 0 {
		t.Fatal("not coalesced")
	}
}
func TestFlushController_TerminalBypassesInterval(t *testing.T) {
	fc := NewFlushController(time.Second)
	first := fc.Schedule(context.Background(), func(context.Context) error { return nil }, false)
	first.Wait(context.Background(), time.Second)
	start := time.Now()
	task := fc.Schedule(context.Background(), func(context.Context) error { return nil }, false)
	fc.Schedule(context.Background(), func(context.Context) error { return nil }, true)
	if !task.Wait(context.Background(), 300*time.Millisecond) || time.Since(start) >= 500*time.Millisecond {
		t.Fatal("terminal did not bypass interval")
	}
}
func TestFlushController_DrainTimeout(t *testing.T) {
	fc := NewFlushController(0)
	release := make(chan struct{})
	fc.Schedule(context.Background(), func(context.Context) error { <-release; return nil }, false)
	if fc.Drain(context.Background(), 100*time.Millisecond) {
		t.Fatal("want timeout")
	}
	if fc.Metrics().TerminalDrainTimeouts != 1 {
		t.Fatal("metric")
	}
	close(release)
}
func TestFlushController_ClosedDropsNonTerminalKeepsTerminal(t *testing.T) {
	fc := NewFlushController(0)
	fc.Close()
	var n atomic.Int32
	fc.Schedule(context.Background(), func(context.Context) error { n.Add(1); return nil }, false).Wait(context.Background(), time.Second)
	task := fc.Schedule(context.Background(), func(context.Context) error { n.Add(1); return nil }, true)
	if !task.Wait(context.Background(), time.Second) || n.Load() != 1 {
		t.Fatalf("runs=%d", n.Load())
	}
}
func TestFlushController_NoLostFinalFrame(t *testing.T) {
	for i := 0; i < 200; i++ {
		fc := NewFlushController(0)
		var final atomic.Bool
		start := make(chan struct{})
		fc.Schedule(context.Background(), func(context.Context) error { close(start); return nil }, false)
		<-start
		task := fc.Schedule(context.Background(), func(context.Context) error { final.Store(true); return nil }, true)
		if !task.Wait(context.Background(), time.Second) || !final.Load() {
			t.Fatalf("iteration %d lost final", i)
		}
	}
}
func TestFlushController_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := NewFlushController(time.Hour)
	first := fc.Schedule(ctx, func(context.Context) error { return nil }, false)
	first.Wait(ctx, time.Second)
	task := fc.Schedule(ctx, func(context.Context) error { return nil }, false)
	cancel()
	if !task.Wait(context.Background(), time.Second) {
		t.Fatal("goroutine did not exit")
	}
}
