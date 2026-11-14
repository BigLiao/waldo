package hostbash

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
)

type Call struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type Result struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Signal    int    `json:"signal,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
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
func Serve(parent context.Context, in io.Reader, out io.Writer, run Runner) error {
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
			reply(req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "waldo-host-bash", "version": "1.0.0"}}, 0, "")
		case "ping":
			reply(req.ID, map[string]any{}, 0, "")
		case "tools/list":
			reply(req.ID, map[string]any{"tools": []any{toolDefinition()}}, 0, "")
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			var call Call
			if json.Unmarshal(req.Params, &p) != nil || p.Name != ToolName {
				reply(req.ID, nil, -32602, "Unknown tool or invalid params")
				continue
			}
			d := json.NewDecoder(bytes.NewReader(p.Arguments))
			d.DisallowUnknownFields()
			if d.Decode(&call) != nil || !validCall(call) {
				reply(req.ID, nil, -32602, "Expected command, optional absolute cwd and timeout_ms (1..600000)")
				continue
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
				reply(req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": string(text)}}, "isError": failed}, 0, "")
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
	return len(c.Command) > 0 && len(c.Command) <= 1<<20 && !bytes.ContainsRune([]byte(c.Command), 0) &&
		(c.Cwd == "" || (c.Cwd[0] == '/' && !bytes.ContainsRune([]byte(c.Cwd), 0))) && c.TimeoutMS >= 0 && c.TimeoutMS <= 600000
}

func toolDefinition() any {
	return map[string]any{
		"name": ToolName,
		"description": `在本机 A 的 Waldo 沙箱中执行 Bash 命令。适用于需要本机依赖、虚拟环境、操作系统、构建测试环境或本机服务的任务。
普通文件处理、文本搜索和使用远端已有工具运行的脚本，优先使用内置 Bash，在远端 B 执行。
两种方式操作同一份项目文件，写入都会影响本机工作区。本工具返回 stdout、stderr 和退出码，不支持交互输入或跨调用保留 Shell 状态。每路输出最多返回 128 KiB，超过时标记截断。`,
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"command":    map[string]any{"type": "string", "minLength": 1, "maxLength": 1 << 20},
			"cwd":        map[string]string{"type": "string", "description": "本机绝对路径；默认启动会话时的工作目录。"},
			"timeout_ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 600000, "default": 120000},
		}, "required": []string{"command"}, "additionalProperties": false},
	}
}
