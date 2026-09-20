//go:build !linux

package bolt

// mmapFlags is zero on platforms without MAP_POPULATE support.
const mmapFlags = 0
