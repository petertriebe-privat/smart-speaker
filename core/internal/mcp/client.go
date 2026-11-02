// Package mcp: a Model Context Protocol client over Streamable HTTP (JSON-RPC POSTs, answers
// as JSON or server-sent events), for Home Assistant and further MCP servers, as
// devices/atoms3r/src/services/{home_assistant,mcp}.cpp.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/netx"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type Client struct {
	URL       string
	Token     string
	Stateless bool // Home Assistant: no initialize, no session
	// Lenient: a failing initialize is not fatal (a stateless server may not need it); the
	// request is sent anyway and the initialize error is reported only if it fails too.
	Lenient bool
	Header  string // header carrying Token, "" = "Authorization: Bearer <token>"
	// MaxReply caps the bytes read of one answer (0 = 4 MiB); a longer one is an error.
	MaxReply int64
	http     *http.Client

	mu         sync.Mutex
	version    string // protocol version the server agreed to, sent as MCP-Protocol-Version
	session    string
	ready      bool
	nextID     atomic.Int64
	ServerName string
}

func NewClient(url, token string, timeout time.Duration) *Client {
	return &Client{URL: url, Token: token, http: netx.Client(timeout)}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Call sends one JSON-RPC request and returns its result.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var initErr error
	if !c.Stateless {
		if err := c.initialize(ctx); err != nil && !c.Lenient {
			return nil, err
		} else if err != nil {
			initErr = err
		}
	}
	result, err := c.post(ctx, method, params, true)
	if err != nil && !c.Stateless && strings.Contains(err.Error(), "HTTP 404") {
		// The server forgot the session (restart): start a new one once.
		c.mu.Lock()
		c.ready, c.session, c.version = false, "", ""
		c.mu.Unlock()
		if err := c.initialize(ctx); err != nil && !c.Lenient {
			return nil, err
		}
		result, err = c.post(ctx, method, params, true)
	}
	if err != nil && initErr != nil {
		return nil, initErr
	}
	return result, err
}

// CloseIdle closes kept connections (an idle TLS connection holds buffers).
func (c *Client) CloseIdle() { c.http.CloseIdleConnections() }

func (c *Client) initialize(ctx context.Context) error {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if ready {
		return nil
	}
	result, err := c.post(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "smart-speaker", "version": "1"},
	}, true)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var info struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	json.Unmarshal(result, &info)
	c.mu.Lock()
	c.ready, c.ServerName, c.version = true, info.ServerInfo.Name, info.ProtocolVersion
	c.mu.Unlock()
	c.post(ctx, "notifications/initialized", nil, false)
	return nil
}

func (c *Client) post(ctx context.Context, method string, params any, expectReply bool) (json.RawMessage, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		body["params"] = params
	}
	if expectReply {
		body["id"] = c.nextID.Add(1)
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		if c.Header != "" {
			req.Header.Set(c.Header, c.Token)
		} else {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.mu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" && method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	c.mu.Unlock()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.mu.Lock()
		c.session = id
		c.mu.Unlock()
	}
	if !expectReply {
		io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if resp.StatusCode != 200 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(text)))
	}
	var reply rpcResponse
	limit := c.MaxReply
	if limit <= 0 {
		limit = 4 << 20
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		reply, err = readSSE(io.LimitReader(resp.Body, limit))
	} else {
		err = json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(&reply)
	}
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, errors.New(reply.Error.Message)
	}
	return reply.Result, nil
}

// readSSE returns the first JSON-RPC response among the events (notifications are skipped).
func readSSE(r io.Reader) (rpcResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if line == "" && data.Len() > 0 {
			var reply rpcResponse
			if json.Unmarshal([]byte(data.String()), &reply) == nil && (reply.Result != nil || reply.Error != nil) {
				return reply, nil
			}
			data.Reset()
		}
	}
	if data.Len() > 0 {
		var reply rpcResponse
		if err := json.Unmarshal([]byte(data.String()), &reply); err == nil {
			return reply, nil
		}
	}
	return rpcResponse{}, errors.New("no answer in the event stream")
}

// ListTools: tools/list (all pages).
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 10; page++ {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		result, err := c.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var list struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &list); err != nil {
			return nil, err
		}
		all = append(all, list.Tools...)
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	return all, nil
}

// CallTool: tools/call; the text parts of the result, or an error text from the server.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (string, error) {
	result, err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, part := range out.Content {
		b.WriteString(part.Text)
	}
	if out.IsError {
		return "", errors.New(b.String())
	}
	return b.String(), nil
}
