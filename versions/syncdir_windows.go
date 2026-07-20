//go:build windows

package versions

// SyncDir is a no-op on Windows. The Win32 API exposes no directory handle that
// can be flushed the way fsync flushes one on a POSIX filesystem, so there is
// nothing to call and reporting an error would fail every write on a platform
// where the caller has no remedy. This is a documented durability gap on Windows,
// not a claim that the rename is already durable.
func SyncDir(string) error { return nil }
