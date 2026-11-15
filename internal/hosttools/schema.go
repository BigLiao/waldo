package hosttools

import (
	"encoding/json"
	"fmt"
)

func Definitions() []any {
	str := func() any { return map[string]any{"type": "string"} }
	num := func() any { return map[string]any{"type": "integer", "minimum": 0} }
	boolean := func() any { return map[string]any{"type": "boolean"} }
	type def struct {
		name, desc string
		props      map[string]any
		required   []string
	}
	defs := []def{
		{"Read", "Read a file. file_path is absolute; offset is 1-based, limit defaults to 2000. Text includes line numbers. Images are returned as images; PDFs use pages (max 20; requires local PDF tools). location auto routes projected project files to host A and agent state/tmp to B. Read before overwriting/editing; versions are tracked internally. Bounded results report has_more.", map[string]any{"file_path": str(), "offset": num(), "limit": map[string]any{"type": "integer", "minimum": 1}, "pages": str(), "allow_large": boolean()}, []string{"file_path"}},
		{"Write", "Write UTF-8 text, creating parent directories. Read an existing file before overwriting it; a changed version is rejected. location selects the filesystem.", map[string]any{"file_path": str(), "content": str()}, []string{"file_path", "content"}},
		{"Edit", "Exact text replacement. Read the file first. old_string must be unique unless replace_all is true. Rejects changes since Read.", map[string]any{"file_path": str(), "old_string": str(), "new_string": str(), "replace_all": boolean()}, []string{"file_path", "old_string", "new_string"}},
		{"Glob", "Find files matching pattern using ripgrep glob syntax, sorted by modification time. path defaults to the session cwd. Results are bounded. Does not follow directory symlinks.", map[string]any{"pattern": str(), "path": str()}, []string{"pattern"}},
		{"Grep", "Search with ripgrep. output_mode defaults to files_with_matches; content supports line numbers and context flags. head_limit defaults to 200; zero disables the line-count limit (byte limit still applies). offset skips output lines. path defaults to session cwd.", map[string]any{"pattern": str(), "path": str(), "glob": str(), "type": str(), "output_mode": map[string]any{"type": "string", "enum": []string{"content", "files_with_matches", "count"}}, "-i": boolean(), "-o": boolean(), "-n": boolean(), "-A": num(), "-B": num(), "-C": num(), "context": num(), "head_limit": num(), "offset": num(), "multiline": boolean()}, []string{"pattern"}},
		{"NotebookEdit", "Edit a Jupyter notebook cell. Read the notebook first. edit_mode defaults to replace; insert requires cell_type and inserts after cell_id (at start when omitted); delete removes cell_id. Cell IDs or zero-based numeric indices are accepted.", map[string]any{"notebook_path": str(), "cell_id": str(), "new_source": str(), "cell_type": map[string]any{"type": "string", "enum": []string{"code", "markdown"}}, "edit_mode": map[string]any{"type": "string", "enum": []string{"replace", "insert", "delete"}}}, []string{"notebook_path", "new_source"}},
	}
	out := []any{}
	for _, d := range defs {
		d.props["location"] = LocationSchema()
		out = append(out, map[string]any{"name": d.name, "description": d.desc, "inputSchema": map[string]any{"type": "object", "properties": d.props, "required": d.required, "additionalProperties": false}, "annotations": map[string]any{"readOnlyHint": d.name == "Read" || d.name == "Grep" || d.name == "Glob"}})
	}
	return out
}
func LocationSchema() any {
	return map[string]any{"type": "string", "enum": []string{"auto", "host", "agent"}, "default": "auto", "description": "auto follows path ownership; host forces A, agent selects B's sandbox. No fallback on missing files. Bash auto means host."}
}

// DecodePublic keeps private worker fields out of the MCP contract.
func DecodePublic(name string, raw []byte) (Args, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Args{}, err
	}
	for _, v := range Definitions() {
		def := v.(map[string]any)
		if def["name"] != name {
			continue
		}
		schema := def["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		for key, value := range fields {
			if props[key] == nil || string(value) == "null" {
				return Args{}, fmt.Errorf("invalid argument %s", key)
			}
		}
		for _, key := range schema["required"].([]string) {
			if _, ok := fields[key]; !ok {
				return Args{}, fmt.Errorf("%s is required", key)
			}
		}
		if name == "Read" {
			if v, ok := fields["limit"]; ok && string(v) == "0" {
				return Args{}, fmt.Errorf("limit must be positive")
			}
		}
		return Decode(name, raw)
	}
	return Args{}, fmt.Errorf("unknown tool")
}
