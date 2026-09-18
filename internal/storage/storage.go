// Package storage implements the Magento cache backends that can be cleaned:
// Cm_Cache_Backend_File, Cm_Cache_Backend_Redis (and Magento's Redis subclass),
// RemoteSynchronizedCache (L2) and Varnish.
package storage

// Storage is a cache backend. Tags and IDs passed in are final keys: already
// prefixed with the id_prefix and (for ids) upper-cased by the caller.
type Storage interface {
	// CleanTags removes every record associated with any of the tags, and the tags themselves.
	CleanTags(tags []string) error
	// CleanIDs removes the records with the given ids.
	CleanIDs(ids []string) error
	// CleanAll flushes the complete backend.
	CleanAll() error
	// Close releases connections. Storage may be kept open and reused across many calls.
	Close() error
	// Describe returns a short human description, e.g. "redis unix:/tmp/r.sock db 15".
	Describe() string
}

// Config is a single cache frontend config from env.php, e.g. cache/frontend/default.
// Keys and nested values are as decoded from PHP/JSON: map[string]any, []any,
// string, int64, float64, bool, nil.
type Config map[string]any
