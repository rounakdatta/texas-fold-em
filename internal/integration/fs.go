package integration

import "os"

// ensureDir is a tiny helper kept separate so it stays trivially
// reviewable. 0o755 matches what the broker's state path uses.
func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
