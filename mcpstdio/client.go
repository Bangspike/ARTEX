// Package mcpstdio provides a bounded stdio MCP transport with cooperative shutdown.
package mcpstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

const MaxFrameBytes = 32 * 1024 * 1024

type RemoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Hooks are per connection. Before can reject or normalize calls; After must
// durably retain evidence before returning its receipt to the caller.
type Hooks struct {
	Allow  func(string) bool
	Before func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	After  func(context.Context, string, json.RawMessage, json.RawMessage) (string, error)
}

type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type Client struct {
	name      string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	writeMu   sync.Mutex
	mu        sync.Mutex
	next      int64
	pending   map[int64]chan response
	done      chan struct{}
	exited    chan struct{}
	readErr   error
	closeOnce sync.Once
	Hooks     Hooks
}

func New(ctx context.Context, name, command string, env map[string]string, args ...string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	c := &Client{name: name, cmd: cmd, stdin: input, stdout: output, pending: map[int64]chan response{}, done: make(chan struct{}), exited: make(chan struct{})}
	go c.read(output)
	// Wait only after stdout has drained; exec.Wait closes its pipes.
	go func() { <-c.done; _ = cmd.Wait(); close(c.exited) }()
	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err = c.RPC(initCtx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "artex", "version": "rea-v1"}})
	if err == nil {
		err = c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), MaxFrameBytes)
	for scanner.Scan() {
		var res response
		if json.Unmarshal(scanner.Bytes(), &res) != nil || res.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch := c.pending[res.ID]
		delete(c.pending, res.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- res
		}
	}
	c.mu.Lock()
	c.readErr = scanner.Err()
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) RPC(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		if res.Error != nil {
			return nil, fmt.Errorf("MCP %d: %s", res.Error.Code, res.Error.Message)
		}
		return res.Result, nil
	case <-ctx.Done():
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": "ARTEX call cancelled"}})
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("MCP frame read: %w", err)
		}
		return nil, errors.New("MCP connection closed")
	}
}

func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, string, error) {
	if c.Hooks.Allow != nil && !c.Hooks.Allow(name) {
		return nil, "", fmt.Errorf("MCP tool %s is unavailable in this analysis", name)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if c.Hooks.Before != nil {
		var err error
		args, err = c.Hooks.Before(ctx, name, args)
		if err != nil {
			return nil, "", err
		}
	}
	raw, err := c.RPC(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, "", err
	}
	var receipt string
	if c.Hooks.After != nil {
		receipt, err = c.Hooks.After(ctx, name, args, raw)
	}
	return raw, receipt, err
}

func (c *Client) Tools(ctx context.Context) ([]tool.CoreTool, error) {
	var out []tool.CoreTool
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.RPC(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var list struct {
			Tools []RemoteTool `json:"tools"`
			Next  string       `json:"nextCursor"`
		}
		if err = json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		for _, rt := range list.Tools {
			if c.Hooks.Allow == nil || c.Hooks.Allow(rt.Name) {
				out = append(out, c.wrap(rt))
			}
		}
		if list.Next == "" {
			return out, nil
		}
		if seen[list.Next] {
			return nil, errors.New("MCP tools/list repeated cursor")
		}
		seen[list.Next] = true
		cursor = list.Next
	}
}

func (c *Client) wrap(rt RemoteTool) tool.CoreTool {
	full := "mcp__" + c.name + "__" + rt.Name
	return tool.Build(tool.Spec{Name: full, Description: rt.Description, Schema: rt.InputSchema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("call MCP tool " + full + "?")
		},
		Run: func(ctx context.Context, args json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			raw, receipt, err := c.Call(ctx, rt.Name, args)
			if err != nil {
				return tool.Errorf(err.Error()), nil
			}
			var res struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if err = json.Unmarshal(raw, &res); err != nil {
				return tool.Errorf("invalid MCP result: " + err.Error()), nil
			}
			var b strings.Builder
			for _, v := range res.Content {
				if v.Type == "text" {
					b.WriteString(v.Text)
				}
			}
			captured := tool.Capture(tc, b.String())
			if receipt != "" {
				captured += "\n" + receipt
			}
			return tool.Result{Content: []llm.ContentBlock{llm.TextBlock(captured)}, IsError: res.IsError}, nil
		}})
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		select {
		case <-c.exited:
			return
		case <-time.After(5 * time.Second):
		}
		_ = c.cmd.Process.Signal(os.Interrupt)
		select {
		case <-c.exited:
			return
		case <-time.After(2 * time.Second):
		}
		_ = c.cmd.Process.Kill()
		_ = c.stdout.Close()
		<-c.exited
	})
	return nil
}
