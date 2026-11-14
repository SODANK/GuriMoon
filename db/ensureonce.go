package db

import "sync"

// ensureGuard runs a pure schema-ensure function at most once per process; a failure is not
// remembered, so the next caller retries. Not for Ensure* helpers that ensure a row or run a
// backfill (EnsureAllianceTables). See docs/db-13-notes.md#schema-ensure-guard.
type ensureGuard struct {
	mu   sync.Mutex
	done bool
}

// do runs fn unless a previous call already succeeded. The mutex serializes
// concurrent first-callers so the DDL cannot be issued twice in parallel; once
// done is set the cost is one uncontended lock.
func (g *ensureGuard) do(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return nil
	}
	if err := fn(); err != nil {
		return err
	}
	g.done = true
	return nil
}
