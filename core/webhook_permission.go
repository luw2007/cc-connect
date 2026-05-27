package core

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// ExternalPermissionRequest is the JSON body for POST /hook/permission.
type ExternalPermissionRequest struct {
	ToolName   string         `json:"tool_name"`
	ToolInput  map[string]any `json:"tool_input"`
	SessionID  string         `json:"session_id,omitempty"`
	Cwd        string         `json:"cwd"`
	Project    string         `json:"project,omitempty"`
	SessionKey string         `json:"session_key,omitempty"`
	Timeout    int            `json:"timeout,omitempty"`
}

// ExternalPermissionResponse is returned by POST /hook/permission.
type ExternalPermissionResponse struct {
	Status   string                `json:"status"`
	Decision *ExternalPermDecision `json:"decision,omitempty"`
	Error    string                `json:"error,omitempty"`
}

// ExternalPermDecision represents the user's allow/deny decision.
type ExternalPermDecision struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

// ExternalNotifyRequest is the JSON body for POST /hook/notify.
type ExternalNotifyRequest struct {
	Event      string `json:"event"`
	Message    string `json:"message"`
	Cwd        string `json:"cwd,omitempty"`
	Project    string `json:"project,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
}

type externalPendingPermission struct {
	RequestID  string
	ToolName   string
	ToolInput  map[string]any
	ChannelKey string // platform:chatID prefix for matching
	CreatedAt  time.Time
	Resolved   chan struct{}
	once       sync.Once
	Decision   *ExternalPermDecision
}

func (ep *externalPendingPermission) resolve(d *ExternalPermDecision) {
	ep.once.Do(func() {
		ep.Decision = d
		close(ep.Resolved)
	})
}

// matchesSessionKey returns true if the given sessionKey belongs to this
// permission's channel. A channelKey "telegram:chat1" matches session keys
// like "telegram:chat1:user1" or "telegram:chat1" itself.
func (ep *externalPendingPermission) matchesSessionKey(sessionKey string) bool {
	if ep.ChannelKey == sessionKey {
		return true
	}
	return strings.HasPrefix(sessionKey, ep.ChannelKey+":")
}

const defaultPermissionTimeout = 600 * time.Second

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "ext-" + hex.EncodeToString(b)
}
