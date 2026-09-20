package bolt

import (
	"time"

	bolt "go.etcd.io/bbolt"
)

// DefaultOptions returns bbolt options tuned for Hetty's workload. It is
// based on bbolt.DefaultOptions, with adjustments for performance and safe
// shutdown behaviour:
//
//   - NoFreelistSync: the freelist is not synced to disk on every commit,
//     which significantly reduces write latency and disk I/O. The freelist
//     is rebuilt from scratch when the database is opened, so no data is
//     lost on close.
//   - FreelistType: the hashmap freelist scales better than the default
//     array freelist for large databases with many pending free pages.
//   - MmapFlags: platform specific mmap flags (e.g. MAP_POPULATE on Linux)
//     to reduce page faults when reading large database files.
//   - Timeout: bounds how long opening the database waits for the file
//     lock, instead of blocking indefinitely.
func DefaultOptions() *bolt.Options {
	opts := *bolt.DefaultOptions

	opts.Timeout = 5 * time.Second
	opts.NoFreelistSync = true
	opts.FreelistType = bolt.FreelistMapType
	opts.MmapFlags = mmapFlags

	return &opts
}
