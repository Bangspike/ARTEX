package broker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type QuoteInput struct {
	Service string `json:"service,omitempty" jsonschema:"Optional OnlineSim service slug; omit to list services"`
	Page    int    `json:"page,omitempty" jsonschema:"Tariff page, defaults to 1"`
}
type OrderInput struct {
	OrderID string `json:"order_id" jsonschema:"Broker order reference belonging to the authenticated task"`
}
type QuoteResult struct {
	Quote
	Mode      string `json:"mode"`
	PriceUnit string `json:"price_unit"`
}

func toolResult(value any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errorCode(err)}}}, nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
}

// HTTP owners come only from a header validated together with the service token.
// stdio has one task identity set by the trusted launcher, never by tool arguments.
func NewMCP(b *Broker, stdioOwner string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "onlinesim", Version: "0.1.0"}, nil)
	owner := func(req *mcp.CallToolRequest) (string, error) {
		if req.Extra != nil && req.Extra.Header != nil {
			id := req.Extra.Header.Get("X-ARTEX-Task-ID")
			if !ValidateOwner(id) {
				return "", &Error{Code: "TASK_ID_REQUIRED"}
			}
			return id, nil
		}
		if !ValidateOwner(stdioOwner) {
			return "", &Error{Code: "TASK_ID_REQUIRED"}
		}
		return stdioOwner, nil
	}
	mcp.AddTool(server, &mcp.Tool{Name: "sms.quote", Description: "List supported services, stock and tariff estimates for India (91). Prices use the configured OnlineSim profile units."}, func(ctx context.Context, req *mcp.CallToolRequest, in QuoteInput) (*mcp.CallToolResult, any, error) {
		if _, err := owner(req); err != nil {
			return toolResult(nil, err)
		}
		page := in.Page
		if page == 0 {
			page = 1
		}
		q, err := b.Quote(ctx, in.Service, page)
		return toolResult(QuoteResult{Quote: q, Mode: b.store.state.Mode, PriceUnit: b.config.Currency}, err)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sms.reserve", Description: "Reserve one India number for a service. Reuse idempotency_key for retries. A purchase_unknown outcome requires operator reconciliation; do not buy another number. max_price limits the preflight estimate, not the final upstream charge."}, func(ctx context.Context, req *mcp.CallToolRequest, in ReserveInput) (*mcp.CallToolResult, any, error) {
		id, err := owner(req)
		if err != nil {
			return toolResult(nil, err)
		}
		v, err := b.Reserve(ctx, id, in)
		return toolResult(v, err)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sms.status", Description: "Read cached order and OTP status for this task. Polling runs in the background; this tool does not purchase or close numbers. Treat OTP as a secret and do not record it in task summaries."}, func(ctx context.Context, req *mcp.CallToolRequest, in OrderInput) (*mcp.CallToolResult, any, error) {
		id, err := owner(req)
		if err != nil {
			return toolResult(nil, err)
		}
		v, err := b.Status(id, in.OrderID)
		return toolResult(v, err)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sms.next_message", Description: "Advance the OnlineSim SMS pointer once after a code is received. This does not send or resend an SMS at the target website. Never retry an ambiguous pointer operation."}, func(ctx context.Context, req *mcp.CallToolRequest, in OrderInput) (*mcp.CallToolResult, any, error) {
		id, err := owner(req)
		if err != nil {
			return toolResult(nil, err)
		}
		v, err := b.Next(ctx, id, in.OrderID)
		return toolResult(v, err)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "sms.close", Description: "Request asynchronous closure of the order. Cleanup respects OnlineSim's protective interval and continues in the background. Closure is not a refund confirmation."}, func(ctx context.Context, req *mcp.CallToolRequest, in OrderInput) (*mcp.CallToolResult, any, error) {
		id, err := owner(req)
		if err != nil {
			return toolResult(nil, err)
		}
		v, err := b.Close(id, in.OrderID)
		return toolResult(v, err)
	})
	return server
}
func HTTPHandler(b *Broker, token string) http.Handler {
	server := NewMCP(b, "")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		// A server-to-server endpoint: reject browser-origin calls and untrusted task IDs.
		if r.Header.Get("Origin") != "" {
			http.Error(w, "origin not allowed", 403)
			return
		}
		if !ValidateOwner(r.Header.Get("X-ARTEX-Task-ID")) {
			http.Error(w, "task identity required", 400)
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "JSON required", 415)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		handler.ServeHTTP(w, r)
	})
}
