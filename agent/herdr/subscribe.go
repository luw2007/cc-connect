package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

const (
	subscribeInitialBackoff = 100 * time.Millisecond
	subscribeMaxBackoff     = 5 * time.Second
)

type subscribeFrame struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type paneStatusData struct {
	PaneID      string `json:"pane_id"`
	AgentStatus string `json:"agent_status"`
}

// runSubscribeReader owns every write to statusCh, including reconnect
// snapshots and streamed frames. Its caller is the only reader. The channel
// has capacity one and publishLatestStatus enforces an exact latest-wins
// contract instead of a best-effort sequence that can discard the newest
// blocked transition under contention.
func (s *herdrSession) runSubscribeReader(ctx context.Context, statusCh chan string, failures chan<- struct{}) {
	backoff := subscribeInitialBackoff
	for ctx.Err() == nil {
		// Every connection attempt starts from authoritative state. Publish the
		// snapshot before reading any stream frame so reconnect gaps cannot keep
		// stale blocked/working state alive.
		info, err := s.client.agentGet(ctx, s.target)
		if err != nil {
			slog.Warn("herdr: subscribe resnapshot failed", "target", s.target, "err", err)
			notifyFailure(failures)
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, subscribeMaxBackoff)
			continue
		}
		if info.AgentStatus != "" && !publishLatestStatus(ctx, statusCh, info.AgentStatus) {
			return
		}
		if info.PaneID == "" {
			notifyFailure(failures)
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, subscribeMaxBackoff)
			continue
		}

		sawFrame, err := s.subscribeOnce(ctx, info.PaneID, statusCh)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("herdr: subscribe stream ended", "target", s.target, "err", err)
		if !sawFrame {
			notifyFailure(failures)
		} else {
			backoff = subscribeInitialBackoff
		}
		if !sleepContext(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, subscribeMaxBackoff)
	}
}

func (s *herdrSession) subscribeOnce(ctx context.Context, paneID string, statusCh chan string) (bool, error) {
	conn, err := s.client.dial(ctx)
	if err != nil {
		return false, fmt.Errorf("herdr: subscribe dial: %w", err)
	}
	defer conn.Close()

	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closed:
		}
	}()

	req := rpcRequest{
		ID:     fmt.Sprintf("sub-%s-%d", s.target, requestCounter.Add(1)),
		Method: "events.subscribe",
		Params: map[string]any{"subscriptions": []map[string]any{
			{"type": "pane.agent_status_changed", "pane_id": paneID},
			{"type": "pane.exited"},
		}},
	}
	line, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("herdr: subscribe marshal: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return false, fmt.Errorf("herdr: subscribe write: %w", err)
	}

	reader := bufio.NewReaderSize(conn, 32*1024)
	ackLine, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("herdr: subscribe ack read: %w", err)
	}
	var ack rpcResponse
	if err := json.Unmarshal([]byte(ackLine), &ack); err != nil {
		return false, fmt.Errorf("herdr: subscribe malformed ack: %w", err)
	}
	if ack.Error != nil {
		return false, fmt.Errorf("herdr: subscribe rejected: %s", ack.Error.Message)
	}

	sawFrame := false
	for {
		frameLine, err := reader.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return sawFrame, ctx.Err()
			}
			return sawFrame, fmt.Errorf("herdr: subscribe read: %w", err)
		}
		var frame subscribeFrame
		if err := json.Unmarshal([]byte(frameLine), &frame); err != nil {
			slog.Warn("herdr: subscribe malformed frame", "target", s.target, "err", err)
			continue
		}
		switch frame.Event {
		case "pane_agent_status_changed", "pane.agent_status_changed":
			var data paneStatusData
			if json.Unmarshal(frame.Data, &data) != nil || data.PaneID != paneID || data.AgentStatus == "" {
				continue
			}
			sawFrame = true
			if !publishLatestStatus(ctx, statusCh, data.AgentStatus) {
				return sawFrame, ctx.Err()
			}
		case "pane_exited", "pane.exited":
			var data paneStatusData
			if json.Unmarshal(frame.Data, &data) != nil || data.PaneID != paneID {
				continue
			}
			sawFrame = true
			if !publishLatestStatus(ctx, statusCh, "done") {
				return sawFrame, ctx.Err()
			}
		}
	}
}

func publishLatestStatus(ctx context.Context, ch chan string, status string) bool {
	for {
		select {
		case ch <- status:
			return true
		case <-ctx.Done():
			return false
		default:
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

func notifyFailure(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
