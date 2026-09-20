//go:build !linux

package bolt

// mmapFlags is a no-op on platforms that don't support MAP_POPULATE.
const mmapFlags = 0
