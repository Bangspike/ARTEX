package mcpstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMCPSubprocess(t *testing.T) {
	if os.Getenv("ARTEX_MCP_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &req)
		if req.ID == 0 {
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "large", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			if strings.Contains(string(req.Params), "wait") {
				continue
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 9<<20)}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		fmt.Println(string(b))
	}
	if path := os.Getenv("ARTEX_MCP_EOF_MARKER"); path != "" {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(path, []byte("clean"), 0600)
	}
	os.Exit(0)
}

func TestLargeResultAndGracefulShutdown(t *testing.T) {
	marker := t.TempDir() + "/closed"
	c, err := New(t.Context(), "rea", os.Args[0], map[string]string{"ARTEX_MCP_HELPER": "1", "ARTEX_MCP_EOF_MARKER": marker}, "-test.run=^TestMCPSubprocess$")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.Tools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	raw, _, err := c.Call(t.Context(), "large", json.RawMessage(`{}`))
	if err != nil || len(raw) < 9<<20 {
		t.Fatalf("large response: %d %v", len(raw), err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err = c.Call(ctx, "wait", nil); err != context.DeadlineExceeded {
		t.Fatalf("cancellation: %v", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "clean" {
		t.Fatalf("child did not complete EOF cleanup: %v", err)
	}
}
