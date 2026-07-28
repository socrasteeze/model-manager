// Package store owns the Phase 0 SQLite database: schema, migrations, and the
// only code permitted to write it.
//
// The database is a single file on purpose (spec §5.1). It is the declared sole
// authority, and a sole authority you cannot copy is a worse failure mode than
// the scattered sidecars this project exists to replace.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a handle on the master database.
type Store struct {
	db *sql.DB

	// SQLite permits exactly one writer. WAL gives unlimited concurrent readers
	// alongside it, which is precisely this app's access pattern -- but the
	// hashing pass runs N goroutines, so writes are serialized here rather than
	// left to collide and retry on SQLITE_BUSY.
	wmu sync.Mutex

	path string
}

// Options configures Open.
type Options struct {
	// AllowNetworkPath disables the §10.5 refusal. It exists for tests and for a
	// user who insists; it is not exposed as a convenience.
	AllowNetworkPath bool

	// Warnf receives non-fatal diagnostics. May be nil.
	Warnf func(format string, args ...any)
}

// Open opens (creating if necessary) the database at path and brings its schema
// up to date.
func Open(path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolving %s: %w", path, err)
	}

	kind, fstype := classifyMount(abs)
	switch kind {
	case MountNetwork:
		if !opts.AllowNetworkPath {
			return nil, &ErrNetworkMount{Path: abs, FSType: fstype}
		}
		if opts.Warnf != nil {
			opts.Warnf("database is on network filesystem %q; corruption is likely (--allow-network-db was set)", fstype)
		}
	case MountUnknown:
		if opts.Warnf != nil {
			opts.Warnf("could not determine the filesystem type for %s; if this is a network share, move the database to a local disk", abs)
		}
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("store: creating database directory: %w", err)
	}

	// WAL + NORMAL is the right pair for a single-writer daemon: durability
	// against process crash is preserved, and the fsync-per-commit cost that
	// would dominate a 19k-row-per-file-commit scan is not paid.
	dsn := "file:" + abs + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", abs, err)
	}
	// One pooled connection. Serializing at the pool avoids two goroutines each
	// holding a connection inside a write transaction and deadlocking on the
	// busy timeout.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connecting to %s: %w", abs, err)
	}

	s := &Store{db: db, path: abs}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close flushes the WAL and closes the database.
func (s *Store) Close() error {
	// Fold the WAL back into the main file so the single-file backup story in
	// spec §16.4 is actually true after a clean shutdown.
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

// Path is the absolute path of the database file.
func (s *Store) Path() string { return s.path }

// DB exposes the handle for read-only query code.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migration (
        version    INTEGER PRIMARY KEY,
        applied_at TEXT NOT NULL
    ) STRICT`); err != nil {
		return fmt.Errorf("store: creating migration table: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migration`).Scan(&current); err != nil {
		return fmt.Errorf("store: reading schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf(
			"store: database schema version %d is newer than this binary understands (%d); upgrade mm",
			current, len(migrations))
	}

	for i := current; i < len(migrations); i++ {
		version := i + 1
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: beginning migration %d: %w", version, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: applying migration %d: %w", version, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migration (version, applied_at) VALUES (?, ?)`,
			version, nowUTC(),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: recording migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: committing migration %d: %w", version, err)
		}
	}
	return nil
}

// SchemaVersion reports the applied schema version.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migration`).Scan(&v)
	return v, err
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
