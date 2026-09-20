package bolt

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

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

// Close syncs pending writes to disk and closes the underlying Bolt
// database. Syncing before close guards against data loss for large
// database files, where dirty pages may still be in the page cache.
func (db *Database) Close() error {
	if err := db.bolt.Sync(); err != nil {
		return fmt.Errorf("bolt: failed to sync database before close: %w", err)
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
