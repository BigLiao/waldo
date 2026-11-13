//go:build !linux

package fswatch

import "fmt"

type Watcher struct{}

func New(string) (*Watcher, error) {
	return nil, fmt.Errorf("project file watching is only supported on Linux")
}

func (*Watcher) Events() <-chan Event { return nil }
func (*Watcher) Close() error         { return nil }
