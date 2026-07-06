package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	// onTransition, if set, is called after every successful state change
	// (via Transition or MarkLocalReady). It must be non-blocking — the
	// events emitter enqueues and returns. Never affects the state machine.
	onTransition func(nzoID, from, to string)
}

// SetTransitionHook installs the outbound-events callback (optional).
func (s *Store) SetTransitionHook(fn func(nzoID, from, to string)) {
	s.onTransition = fn
}

func (s *Store) fireTransition(nzoID, from, to string) {
	if s.onTransition != nil {
		s.onTransition(nzoID, from, to)
	}
}

func Open(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	dsn := "file:" + abs + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }
