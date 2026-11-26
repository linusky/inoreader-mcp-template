package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// ProtocolVersion is the MCP protocol version advertised by this server.
	ProtocolVersion = "2025-03-26"
	maxLineBytes    = 4 * 1024 * 1024 // 4 MiB max incoming JSON-RPC message
)

// ToolHandler executes an MCP tool call and returns a structured result payload.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool defines an MCP tool's metadata and input JSON Schema.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Handler     ToolHandler    `json:"-"`
}

// Server is a lightweight, zero-dependency JSON-RPC 2.0 MCP server over stdio.
type Server struct {
	name    string
	version string
	tools   map[string]Tool
	order   []string
}

// NewServer creates a new MCP server.
func NewServer(name, version string) *Server {
	return &Server{
		name:    name,
		version: version,
		tools:   make(map[string]Tool),
	}
}

// RegisterTool adds a tool to the server.
func (s *Server) RegisterTool(t Tool) {
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
}

// JSON-RPC 2.0 wire types.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type scanItem struct {
	line string
	err  error
	eof  bool
}

// Serve runs the stdio JSON-RPC loop until EOF or context cancellation.
// Only JSON-RPC responses are written to out; diagnostic logs go to errOut.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer, errOut io.Writer) error {
	lines := make(chan scanItem, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
		for scanner.Scan() {
			select {
			case lines <- scanItem{line: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			select {
			case lines <- scanItem{err: err}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case lines <- scanItem{eof: true}:
		case <-ctx.Done():
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item := <-lines:
			if item.eof {
				return nil
			}
			if item.err != nil {
				return fmt.Errorf("stdin read error: %w", item.err)
			}

			line := strings.TrimSpace(item.line)
			if line == "" {
				continue
			}

			var req rpcRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				if werr := writeResponse(out, rpcResponse{
					JSONRPC: "2.0",
					ID:      json.RawMessage("null"),
					Error:   &rpcError{Code: -32700, Message: "parse error"},
				}); werr != nil {
					logWriteErr(errOut, werr)
					return werr
				}
				continue
			}

			idTrimmed := bytes.TrimSpace(req.ID)

			// Reject invalid JSON-RPC version, missing method, or explicit null id.
			if req.JSONRPC != "2.0" || req.Method == "" || string(idTrimmed) == "null" {
				respID := json.RawMessage("null")
				if len(idTrimmed) > 0 && string(idTrimmed) != "null" {
					respID = idTrimmed
				}
				msg := "invalid request"
				switch {
				case req.JSONRPC != "2.0":
					msg = "invalid request: jsonrpc must be 2.0"
				case req.Method == "":
					msg = "invalid request: method is required"
				case string(idTrimmed) == "null":
					msg = "invalid request: id must not be null"
				}
				if werr := writeResponse(out, rpcResponse{
					JSONRPC: "2.0",
					ID:      respID,
					Error:   &rpcError{Code: -32600, Message: msg},
				}); werr != nil {
					logWriteErr(errOut, werr)
					return werr
				}
				continue
			}

			// Notifications omit id and expect no response.
			if len(idTrimmed) == 0 {
				continue
			}

			req.ID = idTrimmed
			resp := s.handleRequest(ctx, req)
			if werr := writeResponse(out, resp); werr != nil {
				logWriteErr(errOut, werr)
				return werr
			}
		}
	}
}

func logWriteErr(errOut io.Writer, err error) {
	if errOut != nil {
		fmt.Fprintf(errOut, "mcp write error: %v\n", err)
	}
}

func (s *Server) handleRequest(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := ProtocolVersion
		if params.ProtocolVersion == "2024-11-05" || params.ProtocolVersion == "2025-03-26" {
			version = params.ProtocolVersion
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": version,
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    s.name,
					"version": s.version,
				},
			},
		}

	case "ping":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}

	case "tools/list":
		list := make([]Tool, 0, len(s.order))
		for _, name := range s.order {
			list = append(list, s.tools[name])
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": list,
			},
		}

	case "tools/call":
		return s.handleToolCall(ctx, req)

	default:
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}
	}
}

func (s *Server) handleToolCall(ctx context.Context, req rpcRequest) rpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: "invalid params: tool name is required"},
		}
	}

	tool, ok := s.tools[params.Name]
	if !ok {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", params.Name)},
		}
	}

	args := bytes.TrimSpace(params.Arguments)
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	} else if args[0] != '{' {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: "invalid params: arguments must be a JSON object"},
		}
	}

	result, err := tool.Handler(ctx, args)
	if err != nil {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": err.Error()},
				},
				"isError": true,
			},
		}
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("failed to serialize result: %v", err)},
				},
				"isError": true,
			},
		}
	}

	return rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(encoded)},
			},
		},
	}
}

func writeResponse(out io.Writer, resp rpcResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = out.Write(data)
	return err
}
