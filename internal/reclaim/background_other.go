//go:build !darwin

package reclaim

// Background is a no-op off macOS; the walk runs at the caller's priority.
func Background() error { return nil }
