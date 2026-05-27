package core

import (
	"sync"
	"testing"
	"time"
)

func TestDebounceQueue_BasicFlush(t *testing.T) {
	var mu sync.Mutex
	var flushed []DebounceMessage
	var flushedScope string

	q := NewDebounceQueue(50*time.Millisecond, func(scope string, batch []DebounceMessage) {
		mu.Lock()
		flushedScope = scope
		flushed = batch
		mu.Unlock()
	})

	q.Push("chat-1", DebounceMessage{Content: "hello"})
	q.Push("chat-1", DebounceMessage{Content: "world"})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if flushedScope != "chat-1" {
		t.Fatalf("scope = %q, want %q", flushedScope, "chat-1")
	}
	if len(flushed) != 2 {
		t.Fatalf("batch size = %d, want 2", len(flushed))
	}
	if flushed[0].Content != "hello" || flushed[1].Content != "world" {
		t.Fatalf("batch = %+v", flushed)
	}
}

func TestDebounceQueue_BlockPreventsFlush(t *testing.T) {
	flushed := make(chan []DebounceMessage, 1)

	q := NewDebounceQueue(30*time.Millisecond, func(_ string, batch []DebounceMessage) {
		flushed <- batch
	})

	q.Block("s1")
	q.Push("s1", DebounceMessage{Content: "during-block"})

	// Wait longer than the debounce window
	time.Sleep(80 * time.Millisecond)

	select {
	case <-flushed:
		t.Fatal("should not flush while blocked")
	default:
	}

	if q.Pending("s1") != 1 {
		t.Fatalf("Pending = %d, want 1", q.Pending("s1"))
	}
}

func TestDebounceQueue_UnblockTriggersFlush(t *testing.T) {
	flushed := make(chan []DebounceMessage, 1)

	q := NewDebounceQueue(30*time.Millisecond, func(_ string, batch []DebounceMessage) {
		flushed <- batch
	})

	q.Block("s1")
	q.Push("s1", DebounceMessage{Content: "msg1"})
	q.Push("s1", DebounceMessage{Content: "msg2"})
	q.Unblock("s1")

	select {
	case batch := <-flushed:
		if len(batch) != 2 {
			t.Fatalf("batch size = %d, want 2", len(batch))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("flush did not fire after unblock")
	}
}

func TestDebounceQueue_Cancel(t *testing.T) {
	q := NewDebounceQueue(50*time.Millisecond, func(_ string, _ []DebounceMessage) {
		t.Fatal("should not flush after cancel")
	})

	q.Push("s1", DebounceMessage{Content: "a"})
	q.Push("s1", DebounceMessage{Content: "b"})

	cancelled := q.Cancel("s1")
	if len(cancelled) != 2 {
		t.Fatalf("cancelled = %d, want 2", len(cancelled))
	}

	time.Sleep(80 * time.Millisecond)
}

func TestDebounceQueue_CancelAll(t *testing.T) {
	q := NewDebounceQueue(50*time.Millisecond, func(_ string, _ []DebounceMessage) {
		t.Fatal("should not flush after CancelAll")
	})

	q.Push("s1", DebounceMessage{Content: "a"})
	q.Push("s2", DebounceMessage{Content: "b"})
	q.CancelAll()

	if q.Pending("s1") != 0 || q.Pending("s2") != 0 {
		t.Fatal("CancelAll should clear all entries")
	}

	time.Sleep(80 * time.Millisecond)
}

func TestDebounceQueue_MultipleScopes(t *testing.T) {
	var mu sync.Mutex
	results := make(map[string]int)

	q := NewDebounceQueue(30*time.Millisecond, func(scope string, batch []DebounceMessage) {
		mu.Lock()
		results[scope] = len(batch)
		mu.Unlock()
	})

	q.Push("a", DebounceMessage{Content: "1"})
	q.Push("b", DebounceMessage{Content: "2"})
	q.Push("b", DebounceMessage{Content: "3"})

	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if results["a"] != 1 {
		t.Fatalf("scope a = %d, want 1", results["a"])
	}
	if results["b"] != 2 {
		t.Fatalf("scope b = %d, want 2", results["b"])
	}
}

func TestDebounceQueue_DebounceResets(t *testing.T) {
	flushed := make(chan int, 1)

	q := NewDebounceQueue(60*time.Millisecond, func(_ string, batch []DebounceMessage) {
		flushed <- len(batch)
	})

	q.Push("s", DebounceMessage{Content: "1"})
	time.Sleep(30 * time.Millisecond)
	q.Push("s", DebounceMessage{Content: "2"})
	time.Sleep(30 * time.Millisecond)
	q.Push("s", DebounceMessage{Content: "3"})

	select {
	case n := <-flushed:
		if n != 3 {
			t.Fatalf("batch = %d, want 3 (debounce should accumulate)", n)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("flush never fired")
	}
}
