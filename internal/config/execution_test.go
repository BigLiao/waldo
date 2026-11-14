package config

import "testing"

func TestExecutionPolicy(t *testing.T) {
	for _, tc := range []struct{ source, project, want string }{
		{"", "/project", "host"},
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
	for _, source := range []string{"[execution]\nmode='other'", "[project.\"/project\".execution]\nmode='REMOTE'"} {
		if _, err := loadCacheConfig(t, source).PolicyFor("/project"); err == nil {
			t.Fatalf("accepted %q", source)
		}
	}
}
