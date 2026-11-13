// Package fswatch observes changes to a project tree. Its events invalidate the
// whole tree; consumers must stop trusting their cache after an error event.
package fswatch

// Event describes a project change. A nil Err invalidates the project cache.
// A non-nil Err means observation is no longer reliable: callers must disable
// caching, rather than treat the error as just another invalidation.
type Event struct {
	Err error
}
