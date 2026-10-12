// Developer-only, line-delimited SDK driver. Replies stay in the harness process memory.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"os"
	"os/exec"
	"time"
)

type request struct {
	Op        string         `json:"op"`
	Container string         `json:"container"`
	Config    string         `json:"config"`
	Name      string         `json:"name"`
	Args      map[string]any `json:"args"`
}

func main() {
	var session *mcp.ClientSession
	defer func() {
		if session != nil {
			session.Close()
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 12*1024*1024)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var r request
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			out.Encode(map[string]any{"ok": false})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout(r))
		var value any
		var err error
		switch r.Op {
		case "connect":
			command := exec.Command("docker", "exec", "-i", r.Container, "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", r.Config)
			command.Stderr = io.Discard
			session, err = mcp.NewClient(&mcp.Implementation{Name: "docker-synthetic-acceptance", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
		case "list":
			if session != nil {
				value, err = session.ListTools(ctx, nil)
			} else {
				err = io.ErrClosedPipe
			}
		case "call":
			if session != nil {
				value, err = session.CallTool(ctx, &mcp.CallToolParams{Name: r.Name, Arguments: r.Args})
			} else {
				err = io.ErrClosedPipe
			}
		case "close":
			if session != nil {
				err = session.Close()
				session = nil
			}
		default:
			err = io.ErrUnexpectedEOF
		}
		cancel()
		out.Encode(map[string]any{"ok": err == nil, "value": value})
	}
}

// Test transport bounds include the existing product media and send envelopes.
func requestTimeout(r request) time.Duration {
	if r.Op == "call" && (r.Name == "send_message" || r.Name == "send_media" || r.Name == "send_voice_note") {
		return 65 * time.Second
	}
	return 15 * time.Second
}
