package config

import "testing"

func TestExecutionPolicy(t *testing.T) {
	for _, tc := range []struct{ source, project, want string }{
		{"", "/project", ""},
		{"[execution]\nmode='host'", "/project", "host"},
		{"[execution]\nmode='host-mcp'", "/project", "host-mcp"},
		{"[execution]\nmode='remote'", "/project", "remote"},
		{"[execution]\nmode='remote'\n[project.\"/project\".execution]\nmode='host'", "/project", "host"},
		{"[execution]\nmode='host'\n[project.\"/project\".execution]\nmode='remote'", "/project", "remote"},
		{"[execution]\nmode='remote'\n[project.\"/project\".execution]\nmode='host'", "/other", "remote"},
	} {
		p, err := loadCacheConfig(t, tc.source).PolicyFor(tc.project)
		if err != nil || p.ExecMode != tc.want {
			t.Fatalf("%q: policy=%+v err=%v", tc.source, p, err)
		}
	}
	if DefaultExecMode(true) != "host-mcp" || DefaultExecMode(false) != "host" {
		t.Fatal("wrong default exec mode")
	}
	for _, source := range []string{"[execution]\nmode='other'", "[project.\"/project\".execution]\nmode='REMOTE'"} {
		if _, err := loadCacheConfig(t, source).PolicyFor("/project"); err == nil {
			t.Fatalf("accepted %q", source)
		}
	}
}
