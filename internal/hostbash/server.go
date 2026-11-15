package hostbash

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"waldo/internal/hosttools"
)

type Call struct {
	Tool           string          `json:"-"`
	Arguments      json.RawMessage `json:"-"`
	Command        string          `json:"command"`
	Cwd            string          `json:"cwd,omitempty"`
	TimeoutMS      int             `json:"timeout,omitempty"`
	Description    string          `json:"description,omitempty"`
	Location       string          `json:"location,omitempty"`
	Background     bool            `json:"run_in_background,omitempty"`
	DisableSandbox bool            `json:"dangerouslyDisableSandbox,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	ShellID        string          `json:"shell_id,omitempty"`
	Block          *bool           `json:"block,omitempty"`
}

type Result struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Signal    int    `json:"signal,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Location  string `json:"location,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Running   bool   `json:"running,omitempty"`
}

type Runner func(context.Context, Call) (Result, error)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Serve implements the small stdio MCP surface needed for one synchronous
// tool. Calls run concurrently so cancellation remains readable on stdin.
// EOF cancels active commands; stdout contains only JSON-RPC messages.
func Serve(parent context.Context, in io.Reader, out io.Writer, run Runner, fileTools ...bool) error {
	files := len(fileTools) > 0 && fileTools[0]
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var writes, mu sync.Mutex
	var workers sync.WaitGroup
	active := map[string]context.CancelFunc{}
	slots := make(chan struct{}, 8)
	encoder := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, code int, message string) {
		response := map[string]any{"jsonrpc": "2.0", "id": id}
		if code != 0 {
			response["error"] = map[string]any{"code": code, "message": message}
		} else {
			response["result"] = result
		}
		writes.Lock()
		err := encoder.Encode(response)
		writes.Unlock()
		if err != nil {
			cancel()
		}
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var req request
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			reply(nil, nil, -32700, "Invalid JSON-RPC message")
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			reply(nil, nil, -32600, "Invalid request")
			continue
		}
		if len(req.ID) == 0 {
			if req.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &p) == nil {
					mu.Lock()
					if stop := active[string(bytes.TrimSpace(p.RequestID))]; stop != nil {
						stop()
					}
					mu.Unlock()
				}
			}
			continue
		}
		var id any
		if json.Unmarshal(req.ID, &id) != nil || id == nil {
			reply(nil, nil, -32600, "Invalid request id")
			continue
		}
		switch id.(type) {
		case string, float64:
		default:
			reply(nil, nil, -32600, "Invalid request id")
			continue
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				reply(req.ID, nil, -32602, "Invalid initialize params")
				continue
			}
			version := p.ProtocolVersion
			switch version {
			case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
			default:
				version = "2025-11-25"
			}
			reply(req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "waldo", "version": "1.0.0"}}, 0, "")
		case "ping":
			reply(req.ID, map[string]any{}, 0, "")
		case "tools/list":
			defs := []any{toolDefinition()}
			if files {
				defs = append(defs, hosttools.Definitions()...)

			}
			defs = append(defs, taskDefinitions()...)
			reply(req.ID, map[string]any{"tools": defs}, 0, "")
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			var call Call
			if json.Unmarshal(req.Params, &p) != nil || (p.Name != ToolName && !(files && hosttools.Known(p.Name)) && p.Name != "TaskOutput" && p.Name != "TaskStop") {
				reply(req.ID, nil, -32602, "Unknown tool or invalid params")
				continue
			}
			if p.Name == ToolName || p.Name == "TaskOutput" || p.Name == "TaskStop" {
				d := json.NewDecoder(bytes.NewReader(p.Arguments))
				d.DisallowUnknownFields()
				if d.Decode(&call) != nil || validateCallFields(p.Name, p.Arguments) != nil || (p.Name == ToolName && !validCall(call)) {
					reply(req.ID, nil, -32602, "Expected native Bash arguments (command, timeout, description, run_in_background), plus location")
					continue
				}
				if p.Name != ToolName {
					call.Tool = p.Name
				}
			} else {
				if _, err := hosttools.DecodePublic(p.Name, p.Arguments); err != nil {
					reply(req.ID, nil, -32602, err.Error())
					continue
				}
				call.Tool = p.Name
				call.Arguments = p.Arguments
			}
			select {
			case slots <- struct{}{}:
			default:
				reply(req.ID, nil, -32000, "Too many active host commands")
				continue
			}
			callCtx, stop := context.WithCancel(ctx)
			key := string(bytes.TrimSpace(req.ID))
			mu.Lock()
			if active[key] != nil {
				mu.Unlock()
				stop()
				<-slots
				reply(req.ID, nil, -32600, "Duplicate request id")
				continue
			}
			active[key] = stop
			mu.Unlock()
			workers.Add(1)
			go func(req request, call Call) {
				defer workers.Done()
				defer func() { stop(); mu.Lock(); delete(active, key); mu.Unlock(); <-slots }()
				res, err := run(callCtx, call)
				text, _ := json.Marshal(res)
				failed := err != nil || res.ExitCode != 0
				if err != nil {
					text, _ = json.Marshal(map[string]any{"error": err.Error(), "output": res})
				}
				reply(req.ID, map[string]any{"content": resultContent(res, text), "isError": failed}, 0, "")
			}(req, call)
		default:
			reply(req.ID, nil, -32601, "Method not found")
		}
	}
	cancel()
	workers.Wait()
	return scanner.Err()
}

func validCall(c Call) bool {
	maxTimeout := 600000
	if c.Background {
		maxTimeout = 7200000
	}
	return c.Command != "" && len(c.Command) <= 1<<20 && !strings.ContainsRune(c.Command, 0) && c.TimeoutMS >= 0 && c.TimeoutMS <= maxTimeout && (c.Location == "" || c.Location == "auto" || c.Location == "host" || c.Location == "agent")
}
func toolDefinition() any {
	return map[string]any{"name": ToolName, "description": "Run a Bash command. Parameters follow Claude Bash, with optional location: host (default/auto) runs in A sandbox; agent runs in B sandbox with B tools and local /tmp. description is explanatory only. Environment variables and cwd do not persist between calls; use absolute paths or cd within the command. Background jobs are owned by this MCP session: poll with its TaskOutput and cancel with its TaskStop. No automatic model wake-up. Sandboxing cannot be disabled.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
		"command": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "number", "minimum": 1, "maximum": 7200000}, "description": map[string]any{"type": "string"}, "run_in_background": map[string]any{"type": "boolean"}, "dangerouslyDisableSandbox": map[string]any{"type": "boolean"}, "location": hosttools.LocationSchema(),
	}, "required": []string{"command"}, "additionalProperties": false}}
}

// Images are separate MCP content blocks, never duplicated in JSON text.
func resultContent(res Result, text []byte) []any {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(res.Stdout), &value) == nil && value["image"] != nil {
		var img map[string]any
		if json.Unmarshal(value["image"], &img) == nil {
			delete(value, "image")
			b, _ := json.Marshal(value)
			res.Stdout = string(b)
			text, _ = json.Marshal(res)
			return []any{map[string]string{"type": "text", "text": string(text)}, img}
		}
	}
	return []any{map[string]string{"type": "text", "text": string(text)}}
}

func validateCallFields(name string, raw []byte) error {
	allowed := map[string]bool{"location": true}
	var names []string
	switch name {
	case ToolName:
		names = []string{"command", "timeout", "description", "run_in_background", "dangerouslyDisableSandbox"}
	case "TaskOutput":
		names = []string{"task_id", "block", "timeout"}
	case "TaskStop":
		names = []string{"task_id", "shell_id"}
	}
	for _, key := range names {
		allowed[key] = true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, value := range fields {
		if !allowed[key] || string(value) == "null" {
			return fmt.Errorf("invalid argument %s", key)
		}
	}
	return nil
}
