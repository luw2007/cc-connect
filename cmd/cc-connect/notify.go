package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

func runNotify(args []string) {
	req, dataDir, err := parseNotifyArgs(args)
	if err != nil {
		if err == errNotifyUsage {
			printNotifyUsage()
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		printNotifyUsage()
		os.Exit(1)
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := client.Post("http://unix/notify", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	fmt.Println("Notification sent.")
}

var errNotifyUsage = fmt.Errorf("show notify usage")

func parseNotifyArgs(args []string) (core.NotifyRequest, string, error) {
	var req core.NotifyRequest
	var dataDir string
	var useStdin bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--type", "-t":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--type requires a value")
			}
			i++
			req.Type = args[i]
		case "--title":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--title requires a value")
			}
			i++
			req.Title = args[i]
		case "--message", "-m":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--message requires a value")
			}
			i++
			req.Message = args[i]
		case "--project", "-p":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--project requires a value")
			}
			i++
			req.Project = args[i]
		case "--session", "-s":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--session requires a value")
			}
			i++
			req.SessionKey = args[i]
		case "--cwd":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--cwd requires a value")
			}
			i++
			req.Cwd = args[i]
		case "--tool-name":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--tool-name requires a value")
			}
			i++
			req.ToolName = args[i]
		case "--tool-input":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--tool-input requires a value")
			}
			i++
			req.ToolInput = args[i]
		case "--stdin":
			useStdin = true
		case "--data-dir":
			if i+1 >= len(args) {
				return req, "", fmt.Errorf("--data-dir requires a value")
			}
			i++
			dataDir = args[i]
		case "--help", "-h":
			return req, "", errNotifyUsage
		}
	}

	if useStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return req, "", fmt.Errorf("reading stdin: %w", err)
		}
		var stdinReq struct {
			NotificationType string `json:"notification_type"`
			Title            string `json:"title"`
			Message          string `json:"message"`
			Cwd              string `json:"cwd"`
			SessionID        string `json:"session_id"`
			ToolName         string `json:"tool_name"`
			ToolInput        string `json:"tool_input"`
		}
		if err := json.Unmarshal(data, &stdinReq); err != nil {
			return req, "", fmt.Errorf("invalid stdin JSON: %w", err)
		}
		if req.Type == "" {
			req.Type = stdinReq.NotificationType
		}
		if req.Title == "" {
			req.Title = stdinReq.Title
		}
		if req.Message == "" {
			req.Message = stdinReq.Message
		}
		if req.Cwd == "" {
			req.Cwd = stdinReq.Cwd
		}
		if req.ToolName == "" {
			req.ToolName = stdinReq.ToolName
		}
		if req.ToolInput == "" {
			req.ToolInput = stdinReq.ToolInput
		}
	}

	if req.Project == "" {
		req.Project = strings.TrimSpace(os.Getenv("CC_PROJECT"))
	}

	if req.Message == "" {
		return req, "", fmt.Errorf("message is required (use --message or --stdin)")
	}

	return req, dataDir, nil
}

func printNotifyUsage() {
	fmt.Println(`Usage: cc-connect notify [options]
       cc-connect notify --stdin < notification.json

Send a notification card to the user via the messaging platform.
Designed to be called from Claude Code's Notification hook.

Options:
  -t, --type <type>        Notification type (permission_prompt, idle_prompt)
      --title <text>       Card title (default: "Claude Code")
  -m, --message <text>     Notification message
  -p, --project <name>     Target project (optional if only one project)
  -s, --session <key>      Target session key (optional, picks first active)
      --cwd <path>         Working directory (for workspace binding resolution)
      --tool-name <name>   Tool name (for permission_prompt context)
      --tool-input <text>  Tool input (for permission_prompt context)
      --stdin              Read notification JSON from stdin (hook mode)
      --data-dir <path>    Data directory (default: ~/.cc-connect)
  -h, --help               Show this help

Hook mode (--stdin) expects JSON with fields:
  notification_type, title, message, cwd, session_id, tool_name, tool_input

Examples:
  cc-connect notify -t idle_prompt -m "Claude finished responding"
  echo '{"notification_type":"permission_prompt","message":"Needs approval","tool_name":"Bash","tool_input":"rm -rf /tmp"}' | cc-connect notify --stdin`)
}
