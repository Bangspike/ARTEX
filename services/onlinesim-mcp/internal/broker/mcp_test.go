package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHTTPMCPProtocolAndTaskIsolation(t *testing.T) {
	b, _, now := fixture(t)
	server := httptest.NewServer(HTTPHandler(b, "fixture-service-token"))
	defer server.Close()
	call := func(task, method string, params any, id int) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequest("POST", server.URL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer fixture-service-token")
		req.Header.Set("X-ARTEX-Task-ID", task)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("HTTP %d: %s", resp.StatusCode, data)
		}
		var out map[string]any
		if e = json.Unmarshal(data, &out); e != nil {
			t.Fatal(e)
		}
		if _, ok := out["error"]; ok {
			t.Fatalf("RPC error: %s", data)
		}
		return out["result"].(map[string]any)
	}
	initialize := call("a", "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "artex-compatible-fixture", "version": "1"}}, 1)
	if initialize["protocolVersion"] != "2025-06-18" {
		t.Fatal(initialize)
	}
	listed := call("a", "tools/list", map[string]any{}, 2)
	if len(listed["tools"].([]any)) != 5 {
		t.Fatal(listed)
	}
	reserveArgs := map[string]any{"service": "demo", "idempotency_key": "one", "max_price": "0.20"}
	getView := func(result map[string]any) View {
		t.Helper()
		if result["isError"] == true {
			t.Fatal(result)
		}
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		var v View
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	r := getView(call("a", "tools/call", map[string]any{"name": "sms.reserve", "arguments": reserveArgs}, 3))
	replay := getView(call("a", "tools/call", map[string]any{"name": "sms.reserve", "arguments": reserveArgs}, 4))
	if r.OrderID != replay.OrderID {
		t.Fatal("RPC replay purchased twice")
	}
	*now = now.Add(3 * time.Second)
	b.Tick(context.Background())
	r = getView(call("a", "tools/call", map[string]any{"name": "sms.status", "arguments": map[string]any{"order_id": r.OrderID}}, 5))
	if r.Code != "123456" {
		t.Fatal(r)
	}
	foreign := call("b", "tools/call", map[string]any{"name": "sms.status", "arguments": map[string]any{"order_id": r.OrderID}}, 6)
	if foreign["isError"] != true {
		t.Fatal("foreign task got OTP")
	}
	getView(call("a", "tools/call", map[string]any{"name": "sms.close", "arguments": map[string]any{"order_id": r.OrderID}}, 7))
	b.Tick(context.Background())
	r = getView(call("a", "tools/call", map[string]any{"name": "sms.status", "arguments": map[string]any{"order_id": r.OrderID}}, 8))
	if r.Status != "closed" || r.Code != "" {
		t.Fatal(r)
	}
}
func TestHTTPAuthenticationAndOrigin(t *testing.T) {
	b, _, _ := fixture(t)
	h := HTTPHandler(b, "fixture-service-token")
	for _, tc := range []struct {
		token, task, origin string
		want                int
	}{{"", "a", "", 401}, {"wrong", "a", "", 401}, {"fixture-service-token", "", "", 400}, {"fixture-service-token", "../other", "", 400}, {"fixture-service-token", "a", "https://untrusted.example", 403}} {
		r := httptest.NewRequest("POST", "http://localhost/mcp", bytes.NewBufferString(`{}`))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("X-ARTEX-Task-ID", tc.task)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: got %d", tc, w.Code)
		}
	}
}
func TestMCPTrustedStdioIdentity(t *testing.T) {
	b, _, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := NewMCP(b, "fixed-task")
	ss, e := server.Connect(ctx, serverTransport, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	cs, e := client.Connect(ctx, clientTransport, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sms.reserve", Arguments: map[string]any{"service": "demo", "idempotency_key": "one", "max_price": "0.20"}})
	if e != nil || result.IsError {
		t.Fatal(result, e)
	}
	var v View
	if e = json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &v); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Status("fixed-task", v.OrderID); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Status("other-task", v.OrderID); errorCode(e) != "ORDER_NOT_FOUND" {
		t.Fatal("stdio identity not enforced")
	}
	bad := NewMCP(b, "")
	badClient, badServer := mcp.NewInMemoryTransports()
	badSS, e := bad.Connect(ctx, badServer, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer badSS.Close()
	badCS, e := client.Connect(ctx, badClient, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer badCS.Close()
	result, e = badCS.CallTool(ctx, &mcp.CallToolParams{Name: "sms.quote", Arguments: map[string]any{}})
	if e != nil || !result.IsError {
		t.Fatal(fmt.Sprint(result), e)
	}
}
