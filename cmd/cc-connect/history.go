package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func runHistory(args []string) {
	var (
		project    string
		sessionKey string
		dataDir    string
		last       int
		quoted     bool
		format     string
	)
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p", "--project":
			if i+1 < len(args) {
				project = args[i+1]
				i++
			}
		case "-s", "--session-key", "--session":
			if i+1 < len(args) {
				sessionKey = args[i+1]
				i++
			}
		case "--data-dir":
			if i+1 < len(args) {
				dataDir = args[i+1]
				i++
			}
		case "-n", "--last":
			if i+1 < len(args) {
				last, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--quoted":
			quoted = true
		case "--json":
			format = "json"
		case "-h", "--help":
			printHistoryUsage()
			return
		default:
			positional = append(positional, args[i])
		}
	}

	if project == "" {
		project = strings.TrimSpace(os.Getenv("CC_PROJECT"))
	}
	if sessionKey == "" {
		sessionKey = strings.TrimSpace(os.Getenv("CC_SESSION_KEY"))
	}

	// positional[0] can be subcommand: "quoted"
	if len(positional) > 0 && positional[0] == "quoted" {
		quoted = true
	}

	sockPath := resolveSocketPath(dataDir)
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: cc-connect is not running (socket not found: %s)\n", sockPath)
		os.Exit(1)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if sessionKey != "" {
		q.Set("session_key", sessionKey)
	}
	if last > 0 {
		q.Set("n", strconv.Itoa(last))
	}
	reqURL := "http://unix/history"
	if len(q) > 0 {
		reqURL += "?" + q.Encode()
	}

	resp, err := client.Get(reqURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: server returned %d\n", resp.StatusCode)
		os.Exit(1)
	}

	var history []core.HistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if len(history) == 0 {
		fmt.Fprintln(os.Stderr, "(no history)")
		return
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(history)
		return
	}

	if quoted {
		printQuoted(history)
		return
	}

	printHistory(history)
}

// printHistory prints history in a human-readable format.
func printHistory(history []core.HistoryEntry) {
	for _, e := range history {
		ts := e.Timestamp.Format("15:04:05")
		role := e.Role
		if role == "user" {
			role = "👤"
		} else {
			role = "🤖"
		}
		content := e.Content
		if len(content) > 200 {
			content = content[:200] + "…"
		}
		fmt.Printf("[%s] %s %s\n", ts, role, content)
	}
}

// printQuoted prints history in a format suitable for agent context injection.
// Each message is prefixed with "Human:" or "Assistant:" for prompt readability.
func printQuoted(history []core.HistoryEntry) {
	var sb strings.Builder
	sb.WriteString("# Recent conversation history\n\n")
	for _, e := range history {
		prefix := "Human"
		if e.Role == "assistant" {
			prefix = "Assistant"
		}
		ts := e.Timestamp.Format(time.RFC3339)
		sb.WriteString(fmt.Sprintf("**%s** (%s):\n%s\n\n", prefix, ts, e.Content))
	}
	fmt.Print(sb.String())
}

func runQuoted(args []string) {
	// Prepend --quoted flag and delegate to runHistory
	runHistory(append([]string{"--quoted"}, args...))
}

func printHistoryUsage() {
	fmt.Println(`Usage: cc-connect history [options]
       cc-connect quoted  [options]

Show conversation history for an active session.

Options:
  -p, --project <name>       Project name (auto-detected from CC_PROJECT env)
  -s, --session-key <key>    Session key  (auto-detected from CC_SESSION_KEY env)
  -n, --last <n>             Show only the last N messages
      --quoted               Output in quoted prompt format (Human:/Assistant: prefixes)
      --json                 Output raw JSON
      --data-dir <path>      Data directory (default: ~/.cc-connect)
  -h, --help                 Show this help

Examples:
  cc-connect history
  cc-connect history -n 5
  cc-connect history --quoted
  cc-connect quoted -n 10
  cc-connect quoted | claude -p "Summarize the conversation above"`)
}
