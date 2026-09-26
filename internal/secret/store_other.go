//go:build !windows

package secret

import "os"

// protect is a no-op outside Windows.
//
// This exists only so the agent can be developed and tested on a developer
// machine. The store is NOT encrypted at rest here: on a non-Windows host the
// protection is the 0600 file mode and the user's home directory. Production
// devices are Windows-only and use the DPAPI path in store_windows.go.
func protect(plaintext []byte) ([]byte, error) { return plaintext, nil }

// unprotect is the no-op counterpart of protect.
func unprotect(sealed []byte) ([]byte, error) { return sealed, nil }

// writePrivate writes b to path with owner-only permissions. The file is
// created with the final mode so it is never briefly world-readable.
func writePrivate(path string, b []byte) error {
	return os.WriteFile(path, b, 0o600)
}

// Protection describes how this platform protects the blob, for the enrollment
// output. Saying "DPAPI" on a developer machine would be a lie.
func (s *Store) Protection() string {
	return "0600 file (developer platform: use the Windows agent for real deployments)"
}
