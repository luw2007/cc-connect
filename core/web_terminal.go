package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
)

// outboundIP returns the preferred outbound IP of this machine (first non-loopback).
// Falls back to 127.0.0.1 if detection fails.
func outboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// TerminalServer runs one HTTP server per active terminal session, exposing
// a WebSocket endpoint that proxies the tmux pty stream so a browser xterm.js
// client can attach to a live terminal.
type TerminalServer struct {
	mu       sync.Mutex
	sessions map[string]*terminalEntry // token → entry
}

type terminalEntry struct {
	token    string
	attacher TerminalAttacher
	server   *http.Server
	listener net.Listener
	port     int
}

var globalTerminalServer = &TerminalServer{
	sessions: make(map[string]*terminalEntry),
}

// GetTerminalServer returns the process-wide TerminalServer singleton.
func GetTerminalServer() *TerminalServer {
	return globalTerminalServer
}

// Register starts a new terminal HTTP server for the given attacher and
// returns the URL (http://localhost:<port>/terminal/<token>).
func (ts *TerminalServer) Register(attacher TerminalAttacher) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	token := GenerateToken(16)

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return "", fmt.Errorf("web terminal: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	host := outboundIP()

	entry := &terminalEntry{
		token:    token,
		attacher: attacher,
		listener: ln,
		port:     port,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/terminal/"+token, func(w http.ResponseWriter, r *http.Request) {
		ts.handleWS(w, r, entry)
	})
	mux.HandleFunc("/terminal/"+token+"/resize", func(w http.ResponseWriter, r *http.Request) {
		ts.handleResize(w, r, entry)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/terminal/"+token, http.StatusFound)
	})

	srv := &http.Server{Handler: mux}
	entry.server = srv

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Debug("web terminal server closed", "token", token, "error", err)
		}
	}()

	ts.sessions[token] = entry
	termURL := fmt.Sprintf("http://%s:%d/terminal/%s", host, port, token)
	slog.Info("web terminal started", "port", port, "host", host)
	return termURL, nil
}

// Unregister stops and removes the terminal server for the given token.
func (ts *TerminalServer) Unregister(token string) {
	ts.mu.Lock()
	entry, ok := ts.sessions[token]
	if ok {
		delete(ts.sessions, token)
	}
	ts.mu.Unlock()
	if ok && entry.server != nil {
		_ = entry.server.Close()
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		h := u.Hostname()
		if h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return true
		}
		// Allow private network ranges (10.x, 172.16-31.x, 192.168.x)
		ip := net.ParseIP(h)
		return ip != nil && ip.IsPrivate()
	},
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func (ts *TerminalServer) handleWS(w http.ResponseWriter, r *http.Request, entry *terminalEntry) {
	// Serve the xterm.js HTML page for regular browser GETs
	if r.Header.Get("Upgrade") != "websocket" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, terminalHTML, entry.token)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("web terminal: ws upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	rows, cols := entry.attacher.TerminalSize()
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	_ = entry.attacher.ResizeTerminal(rows, cols)

	pty, err := entry.attacher.AttachTerminal()
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\nfailed to attach terminal: "+err.Error()+"\r\n"))
		return
	}
	defer pty.Close()

	// pty → websocket; close conn on exit to unblock the read loop below
	go func() {
		defer conn.Close()
		buf := make([]byte, 4096)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				if err2 := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err2 != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// websocket → pty (browser keystrokes)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if _, err := pty.Write(msg); err != nil {
			return
		}
	}
}

func (ts *TerminalServer) handleResize(w http.ResponseWriter, r *http.Request, entry *terminalEntry) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Rows int `json:"rows"`
		Cols int `json:"cols"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Rows == 0 || body.Cols == 0 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	_ = entry.attacher.ResizeTerminal(body.Rows, body.Cols)
	w.WriteHeader(http.StatusNoContent)
}

// terminalHTML is a self-contained xterm.js page served at the terminal URL.
const terminalHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>cc-connect terminal</title>
<style>
* { margin:0; padding:0; box-sizing:border-box; }
body { background:#1e1e1e; display:flex; flex-direction:column; height:100vh; }
#toolbar { background:#2d2d2d; padding:6px 10px; display:flex; gap:6px; flex-wrap:wrap; }
button { background:#444; color:#ccc; border:none; border-radius:3px; padding:4px 8px; cursor:pointer; font-size:13px; }
button:active { background:#666; }
#terminal { flex:1; }
</style>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5/css/xterm.css"/>
<script src="https://cdn.jsdelivr.net/npm/xterm@5/lib/xterm.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8/lib/xterm-addon-fit.js"></script>
</head>
<body>
<div id="toolbar">
  <button onclick="term.write('\x03')">^C</button>
  <button onclick="term.write('\x1b')">Esc</button>
  <button onclick="term.write('\t')">Tab</button>
  <button onclick="term.write('\r')">Enter</button>
  <button onclick="term.write('\x1b[A')">↑</button>
  <button onclick="term.write('\x1b[B')">↓</button>
  <button onclick="term.write('\x1b[C')">→</button>
  <button onclick="term.write('\x1b[D')">←</button>
</div>
<div id="terminal"></div>
<script>
const token = "%s";
const term = new Terminal({ fontFamily: "monospace", fontSize: 14, theme: { background: "#1e1e1e" } });
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("terminal"));
fit.fit();

const proto = location.protocol === "https:" ? "wss" : "ws";
const ws = new WebSocket(proto + "://" + location.host + "/terminal/" + token);
ws.binaryType = "arraybuffer";
ws.onmessage = e => term.write(new Uint8Array(e.data));
ws.onclose = () => term.write("\r\n\x1b[31m[disconnected]\x1b[0m\r\n");

term.onData(data => { if (ws.readyState === 1) ws.send(new TextEncoder().encode(data)); });

window.addEventListener("resize", () => {
  fit.fit();
  const dims = fit.proposeDimensions();
  if (dims) fetch("/terminal/" + token + "/resize", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({rows: dims.rows, cols: dims.cols})
  });
});
</script>
</body>
</html>`
