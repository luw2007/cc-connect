package core

import (
	"context"
	"testing"
)

func TestWithTrace_GeneratesTraceID(t *testing.T) {
	ctx := WithTrace(context.Background(), TraceContext{
		SessionKey: "sess-1",
		Platform:   "feishu",
	})
	tc := TraceFrom(ctx)
	if tc == nil {
		t.Fatal("expected TraceContext, got nil")
	}
	if tc.TraceID == "" {
		t.Fatal("expected auto-generated TraceID")
	}
	if len(tc.TraceID) != 8 {
		t.Fatalf("TraceID length = %d, want 8", len(tc.TraceID))
	}
	if tc.SessionKey != "sess-1" {
		t.Fatalf("SessionKey = %q, want %q", tc.SessionKey, "sess-1")
	}
	if tc.Platform != "feishu" {
		t.Fatalf("Platform = %q, want %q", tc.Platform, "feishu")
	}
}

func TestWithTrace_PreservesExplicitTraceID(t *testing.T) {
	ctx := WithTrace(context.Background(), TraceContext{
		TraceID: "deadbeef",
		UserID:  "user-42",
	})
	tc := TraceFrom(ctx)
	if tc == nil {
		t.Fatal("expected TraceContext, got nil")
	}
	if tc.TraceID != "deadbeef" {
		t.Fatalf("TraceID = %q, want %q", tc.TraceID, "deadbeef")
	}
}

func TestTraceFrom_ReturnsNilWithoutContext(t *testing.T) {
	tc := TraceFrom(context.Background())
	if tc != nil {
		t.Fatalf("expected nil, got %+v", tc)
	}
}

func TestTlog_CachesLogger(t *testing.T) {
	ctx := WithTrace(context.Background(), TraceContext{
		TraceID:    "abc12345",
		SessionKey: "s1",
		Platform:   "telegram",
		MsgID:      "msg-99",
	})
	l1 := Tlog(ctx)
	l2 := Tlog(ctx)
	if l1 != l2 {
		t.Fatal("Tlog should return the same cached logger instance")
	}
}

func TestTlog_WithoutContext(t *testing.T) {
	l := Tlog(context.Background())
	if l == nil {
		t.Fatal("Tlog returned nil for bare context")
	}
}

func TestNewTraceID_Unique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id := NewTraceID()
		if seen[id] {
			t.Fatalf("duplicate TraceID after %d iterations: %s", i, id)
		}
		seen[id] = true
	}
}
