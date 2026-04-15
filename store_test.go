package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	if s.Get().Seeded() {
		t.Fatal("fresh store should be unseeded")
	}

	want := State{
		RefreshToken: "rt-xxx",
		AccessToken:  "at-yyy",
		ExpiresAt:    time.Now().Add(15 * time.Minute).UTC().Truncate(time.Millisecond),
		DeviceHash:   "00000000-0000-0000-0000-000000000000",
		UpdatedAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.Set(want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !s.Get().Seeded() {
		t.Fatal("store should be seeded after Set")
	}

	// Reopen from disk — should load the same thing.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := s2.Get()
	if got.RefreshToken != want.RefreshToken || got.DeviceHash != want.DeviceHash {
		t.Fatalf("reload mismatch: got %+v want %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("ExpiresAt: got %v want %v", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestStore_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(State{RefreshToken: "x", DeviceHash: "y"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := info.Mode().Perm(); m != 0o600 {
		t.Fatalf("state file perms: got %o want 0600", m)
	}
}

func TestStore_MissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore should tolerate missing file: %v", err)
	}
	if s.Get().Seeded() {
		t.Fatal("expected unseeded state")
	}
}

func TestStore_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			if err := s.Set(State{
				RefreshToken: "rt",
				AccessToken:  "at",
				DeviceHash:   "dh",
				ExpiresAt:    time.Now().Add(time.Duration(i) * time.Minute),
				UpdatedAt:    time.Now(),
			}); err != nil {
				t.Errorf("Set %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Re-open to make sure the file is well-formed JSON (not corrupted
	// by interleaved writes).
	if _, err := OpenStore(path); err != nil {
		t.Fatalf("concurrent writes left file corrupted: %v", err)
	}
}

func TestStore_AtomicRenameLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := s.Set(State{RefreshToken: "rt", DeviceHash: "dh", UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "state.json" {
			continue
		}
		t.Fatalf("unexpected leftover file in state dir: %s", e.Name())
	}
}
