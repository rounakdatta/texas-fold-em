package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is everything the broker needs to serve access tokens.
//
// It's persisted as a single JSON file on disk at Config.StatePath. Writes
// are atomic (temp-file + fsync + rename) so a power loss mid-write can
// never corrupt the refresh-token chain — either the old state survives,
// or the new one does, never a half-file.
type State struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	DeviceHash   string    `json:"device_hash"`
	UpdatedAt    time.Time `json:"updated_at"`

	// RejectedAt is set when Fold has invalidated the refresh chain
	// (401 on /tokens/refresh). Once set, the broker refuses to call
	// /tokens/refresh until /init provides a fresh seed. This prevents
	// the keep-warm loop from hammering Fold after a revocation —
	// every retry would fail identically anyway. The flag is persisted
	// so a restart also respects the tombstone.
	RejectedAt     *time.Time `json:"rejected_at,omitempty"`
	RejectedReason string     `json:"rejected_reason,omitempty"`
}

// Seeded reports whether the state has enough to refresh — i.e. the broker
// has been initialised at least once. Returns true even when the chain has
// since been rejected; callers that care about "actually usable" should
// check !Seeded() || Rejected().
func (s State) Seeded() bool {
	return s.RefreshToken != "" && s.DeviceHash != ""
}

// Rejected reports whether Fold has told us this refresh chain is dead.
// Cleared only by a successful /init.
func (s State) Rejected() bool {
	return s.RejectedAt != nil
}

// Store owns the on-disk state file and serialises all writes through a
// single RWMutex. Reads are cheap (RLock + copy).
type Store struct {
	path string

	mu    sync.RWMutex
	state State
}

// OpenStore creates the state directory if missing, loads any existing
// state from disk, and returns a ready-to-use Store. A missing state file
// is NOT an error — it means "not yet seeded".
func OpenStore(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %q: %w", dir, err)
	}

	s := &Store{path: path}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// fresh install — leave zero state
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read state %q: %w", path, err)
	}

	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state %q: %w", path, err)
	}
	return s, nil
}

// Get returns a by-value snapshot of the current state. Safe to call
// concurrently.
func (s *Store) Get() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Set atomically persists next to disk and then updates the in-memory copy.
// On any error the in-memory state is unchanged, so readers keep seeing
// the last known-good state.
func (s *Store) Set(next State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := writeFileAtomic(s.path, buf, 0o600); err != nil {
		return err
	}
	s.state = next
	return nil
}

// writeFileAtomic writes data to a sibling tempfile in the same directory
// as path, fsyncs it, and renames it over path. A concurrent crash at any
// point leaves either the old file or the new file — never a partial file.
//
// Mode is applied to the tempfile before rename (atomic rename preserves
// permissions).
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	// best-effort cleanup if we don't reach rename
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename tempfile -> %q: %w", path, err)
	}

	// fsync the directory so the rename is durable across a crash.
	// Best-effort: not all filesystems require or support this, and we've
	// already done the work, so don't fail on directory-fsync errors.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
