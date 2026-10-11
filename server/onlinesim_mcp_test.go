package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Opt-in integration test against the standalone executable. The command always
// selects mock mode and clears the provider API Key. It never buys real numbers.
func TestOnlineSimMCPMockIntegration(t *testing.T) {
	binary := os.Getenv("ONLINESIM_MOCK_BINARY")
	if binary == "" {
		t.Skip("build services/onlinesim-mcp and set ONLINESIM_MOCK_BINARY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, transport := range []string{"http", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			server := &db.MCPServer{Name: "onlinesim", Transport: transport, Enabled: true}
			if transport == "stdio" {
				server.Command = binary
				server.Args, _ = json.Marshal([]string{"-mode", "mock", "-transport", "stdio", "-data-dir", t.TempDir(), "-poll-interval", "100ms"})
				server.Env = json.RawMessage(`{"ARTEX_TASK_ID":"{{task_id}}","ONLINESIM_API_KEY":""}`)
			} else {
				listener, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				address := listener.Addr().String()
				listener.Close()
				cmd := exec.CommandContext(ctx, binary, "-mode", "mock", "-transport", "http", "-listen", address, "-data-dir", t.TempDir(), "-poll-interval", "100ms")
				cmd.Env = append(os.Environ(), "ONLINESIM_MCP_TOKEN=mock-integration-token", "ONLINESIM_API_KEY=")
				if e = cmd.Start(); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() {
					if cmd.Process != nil {
						cmd.Process.Kill()
					}
					cmd.Wait()
				})
				server.URL = "http://" + address + "/mcp"
				server.Env = json.RawMessage(`{"Authorization":"Bearer mock-integration-token","X-ARTEX-Task-ID":"{{task_id}}"}`)
				client := &http.Client{Timeout: time.Second}
				ready := false
				for i := 0; i < 30; i++ {
					resp, e := client.Get("http://" + address + "/healthz")
					if e == nil {
						resp.Body.Close()
						if resp.StatusCode == 200 {
							ready = true
							break
						}
					}
					time.Sleep(50 * time.Millisecond)
				}
				if !ready {
					t.Fatal("mock service did not start")
				}
			}
			cl, e := connectMCP(agent.WithRunInfo(ctx, agent.RunInfo{TaskID: 47}), server)
			if e != nil {
				t.Fatal(e)
			}
			defer cl.Close()
			tools, e := cl.Tools(ctx)
			if e != nil || len(tools) != 5 {
				t.Fatal(len(tools), e)
			}
			call := func(name string, args any) map[string]any {
				t.Helper()
				data, _ := json.Marshal(args)
				for _, tool := range tools {
					if tool.Name() == "mcp__onlinesim__"+name {
						r, e := tool.Call(ctx, data, &actool.ToolContext{MaxOutputChars: 100000})
						if e != nil || r.IsError || len(r.Content) != 1 {
							t.Fatalf("%s: %v %v", name, r.IsError, e)
						}
						var result map[string]any
						if e = json.Unmarshal([]byte(r.Content[0].Text), &result); e != nil {
							t.Fatal(e)
						}
						return result
					}
				}
				t.Fatal("missing tool", name)
				return nil
			}
			args := map[string]any{"service": "demo", "idempotency_key": "attempt-1", "max_price": "0.20"}
			order := call("sms.reserve", args)
			replay := call("sms.reserve", args)
			if order["order_id"] != replay["order_id"] || order["mode"] != "mock" {
				t.Fatal("reserve replay failed")
			}
			id := map[string]any{"order_id": order["order_id"]}
			received := false
			for i := 0; i < 40; i++ {
				s := call("sms.status", id)
				if s["status"] == "code_received" {
					received = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !received {
				t.Fatal("mock OTP not delivered")
			}
			pending := call("sms.close", id)
			if pending["close_requested"] != true {
				t.Fatal("cleanup not queued")
			}
			closed := false
			for i := 0; i < 20; i++ {
				s := call("sms.status", id)
				if s["status"] == "closed" {
					closed = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if !closed {
				t.Fatal("mock cleanup did not finish")
			}
			t.Log(fmt.Sprintf("%s: ARTEX connector negotiated five tools, replayed one order, received mock OTP and closed it", transport))
		})
	}
}
