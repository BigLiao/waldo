package hostbash

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"waldo/internal/hosttools"
)

type job struct {
	done     chan struct{}
	cancel   context.CancelFunc
	result   Result
	err      error
	location string
}

func taskDefinitions() []any {
	properties := func() map[string]any {
		return map[string]any{"task_id": map[string]any{"type": "string"}, "location": hosttools.LocationSchema()}
	}
	output := properties()
	output["block"] = map[string]any{"type": "boolean", "default": true}
	output["timeout"] = map[string]any{"type": "number", "minimum": 0, "maximum": 300000, "default": 30000}
	stop := properties()
	stop["shell_id"] = map[string]any{"type": "string", "description": "Deprecated alias for task_id"}
	return []any{
		map[string]any{"name": "TaskOutput", "description": "Get output from a Waldo background Bash task. block defaults to true, timeout to 30000 ms. Only tasks started by this MCP session are supported; no automatic completion notification.", "inputSchema": map[string]any{"type": "object", "properties": output, "required": []string{"task_id"}, "additionalProperties": false}},
		map[string]any{"name": "TaskStop", "description": "Stop a Waldo background Bash task in its original location.", "inputSchema": map[string]any{"type": "object", "properties": stop, "additionalProperties": false}},
	}
}

func (r *routedRunner) background(c Call) (Result, error) {
	if c.DisableSandbox {
		return Result{}, fmt.Errorf("dangerouslyDisableSandbox is unsupported")
	}
	if !validCall(c) {
		return Result{}, fmt.Errorf("invalid Bash arguments")
	}
	if c.TimeoutMS == 0 {
		c.TimeoutMS = 1800000
	}
	side := "host"
	if c.Location == "agent" {
		side = "agent"
	}
	var bytes [12]byte
	if _, e := rand.Read(bytes[:]); e != nil {
		return Result{}, e
	}
	id := "waldo-" + hex.EncodeToString(bytes[:])
	r.mu.Lock()
	active := 0
	for _, j := range r.jobs {
		select {
		case <-j.done:
		default:
			active++
		}
	}
	if active >= 8 {
		r.mu.Unlock()
		return Result{}, fmt.Errorf("at most 8 background tasks may run")
	}
	if len(r.jobs) >= 64 {
		for key, j := range r.jobs {
			select {
			case <-j.done:
				delete(r.jobs, key)
			default:
			}
			if len(r.jobs) < 64 {
				break
			}
		}
	}
	ctx, cancel := context.WithCancel(r.ctx)
	j := &job{done: make(chan struct{}), cancel: cancel, location: side}
	r.jobs[id] = j
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer cancel()
		j.result, j.err = r.execute(ctx, c)
		j.result.TaskID = id
		close(j.done)
	}()
	return Result{TaskID: id, Running: true, Location: side}, nil
}

func (r *routedRunner) task(ctx context.Context, c Call) (Result, error) {
	id := c.TaskID
	if id == "" && c.Tool == "TaskStop" {
		id = c.ShellID
	}
	if id == "" {
		return Result{}, fmt.Errorf("task_id is required")
	}
	if c.Location != "" && c.Location != "auto" && c.Location != "host" && c.Location != "agent" {
		return Result{}, fmt.Errorf("invalid location")
	}
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j == nil {
		return Result{}, fmt.Errorf("unknown task_id in this MCP session")
	}
	if c.Location != "" && c.Location != "auto" && c.Location != j.location {
		return Result{}, fmt.Errorf("task belongs to location=%s", j.location)
	}
	if c.Tool == "TaskStop" {
		j.cancel()
		select {
		case <-j.done:
			return Result{TaskID: id, Location: j.location, Stdout: "Task stopped"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if c.TimeoutMS < 0 || c.TimeoutMS > 300000 {
		return Result{}, fmt.Errorf("timeout must be between 0 and 300000 ms")
	}
	if c.Block == nil || *c.Block {
		timeout := c.TimeoutMS
		if timeout == 0 {
			timeout = 30000
		}
		timer := time.NewTimer(time.Duration(timeout) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-j.done:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-timer.C:
		}
	}
	select {
	case <-j.done:
		return j.result, j.err
	default:
		return Result{TaskID: id, Running: true, Location: j.location}, nil
	}
}
