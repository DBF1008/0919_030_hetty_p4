package bolt

import (
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DefaultOptions returns bbolt options tuned for Hetty's workload. Compared
// to bbolt's defaults, it avoids blocking indefinitely on file locks, skips
// syncing the freelist on every commit (it's rebuilt on open), uses a
// hashmap based freelist for better performance with large databases, and
// passes platform specific mmap flags (see mmapflags_*.go).
func DefaultOptions() *bolt.Options {
	opts := *bolt.DefaultOptions

	// Fail fast instead of blocking indefinitely when the database file is
	// locked by another process.
	opts.Timeout = 1 * time.Second

	// Don't fsync the freelist on every commit. The freelist is rebuilt
	// when the database is opened, so this is safe and significantly speeds
	// up commits on large databases.
	opts.NoFreelistSync = true

	// Use an in-memory hashmap for the freelist, which performs better than
	// the default array type for databases with many free pages.
	opts.FreelistType = bolt.FreelistMapType

	// Platform specific mmap flags (e.g. MAP_POPULATE on Linux).
	opts.MmapFlags = mmapFlags

	return &opts
}

// Database is used to store and retrieve data from an underlying Bolt database.
type Database struct {
	bolt *bolt.DB
}

// OpenDatabase opens a new Bolt database.
func OpenDatabase(path string, opts *bolt.Options) (*Database, error) {
	db, err := bolt.Open(path, 0o600, opts)
	if err != nil {
		return nil, fmt.Errorf("bolt: failed to open database: %w", err)
	}

	return DatabaseFromBoltDB(db)
}

// Close closes the underlying Bolt database.
func (db *Database) Close() error {
	// Sync pending writes to disk before closing, to guard against data
	// loss on large databases.
	if err := db.bolt.Sync(); err != nil {
		return fmt.Errorf("bolt: failed to sync database: %w", err)
	}

	return db.bolt.Close()
}

// DatabaseFromBoltDB returns a Database with `db` set as the underlying Bolt
// database.
func DatabaseFromBoltDB(db *bolt.DB) (*Database, error) {
	err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(projectsBucketName)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bolt: failed to create projects bucket: %w", err)
	}

	return &Database{bolt: db}, nil
}

func createNestedBucket(tx *bolt.Tx, names ...[]byte) (b *bolt.Bucket, err error) {
	for i, name := range names {
		if b == nil {
			b, err = tx.CreateBucketIfNotExists(name)
		} else {
			b, err = b.CreateBucketIfNotExists(name)
		}
		if err != nil {
			return nil, fmt.Errorf("bolt: failed to create nested bucket %q: %w", names[:i+1], err)
		}
	}

	return b, nil
}
