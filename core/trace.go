package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
)

type traceCtxKey struct{}

// TraceContext carries per-request context for structured logging.
// All fields propagate automatically through context.Context.
type TraceContext struct {
	TraceID    string
	SessionKey string
	Platform   string
	UserID     string
	MsgID      string

	once   sync.Once
	logger *slog.Logger
}

// WithTrace returns a child context carrying the given TraceContext.
// If tc.TraceID is empty, a new one is generated.
func WithTrace(ctx context.Context, tc TraceContext) context.Context {
	if tc.TraceID == "" {
		tc.TraceID = NewTraceID()
	}
	return context.WithValue(ctx, traceCtxKey{}, &tc)
}

// TraceFrom extracts the TraceContext from ctx, or returns nil.
func TraceFrom(ctx context.Context) *TraceContext {
	tc, _ := ctx.Value(traceCtxKey{}).(*TraceContext)
	return tc
}

// NewTraceID generates an 8-char hex trace identifier.
func NewTraceID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Tlog returns a slog.Logger enriched with trace fields from ctx.
// The logger is cached on first call per TraceContext — safe for hot paths.
// Falls back to slog.Default() if no trace is present.
func Tlog(ctx context.Context) *slog.Logger {
	tc := TraceFrom(ctx)
	if tc == nil {
		return slog.Default()
	}
	tc.once.Do(func() {
		attrs := make([]any, 0, 10)
		attrs = append(attrs, "trace_id", tc.TraceID)
		if tc.SessionKey != "" {
			attrs = append(attrs, "session", tc.SessionKey)
		}
		if tc.Platform != "" {
			attrs = append(attrs, "platform", tc.Platform)
		}
		if tc.UserID != "" {
			attrs = append(attrs, "user_id", tc.UserID)
		}
		if tc.MsgID != "" {
			attrs = append(attrs, "msg_id", tc.MsgID)
		}
		tc.logger = slog.With(attrs...)
	})
	return tc.logger
}
