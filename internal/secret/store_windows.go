//go:build windows

package secret

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// entropy binds the blob to this application. It is not a secret: it lives in
// the binary. Its job is domain separation, so a blob produced by an unrelated
// DPAPI consumer on the same machine fails to unseal rather than silently
// yielding garbage.
var entropy = []byte("SoftafriqueBackupAgent/credentials/v1")

// protect seals plaintext with DPAPI.
//
// CRYPTPROTECT_LOCAL_MACHINE is required, not a shortcut: the service runs as
// LocalSystem, so there is no user profile to scope the key to, and a
// user-scoped blob could not be decrypted at all. The consequence is that the
// blob is decryptable by anything running as SYSTEM or an administrator on that
// machine, which is exactly the boundary the ProgramData ACL enforces.
//
// CRYPTPROTECT_UI_FORBIDDEN keeps it non-interactive; without it a decrypt
// attempt can pop a dialog on a customer's desktop.
func protect(plaintext []byte) ([]byte, error) {
	var in windows.DataBlob
	if len(plaintext) > 0 {
		in.Size = uint32(len(plaintext))
		in.Data = &plaintext[0]
	}
	var ent windows.DataBlob
	ent.Size = uint32(len(entropy))
	ent.Data = &entropy[0]

	var out windows.DataBlob
	flags := uint32(windows.CRYPTPROTECT_LOCAL_MACHINE | windows.CRYPTPROTECT_UI_FORBIDDEN)
	if err := windows.CryptProtectData(&in, nil, &ent, 0, nil, flags, &out); err != nil {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	return copyAndFree(out)
}

// unprotect opens a blob sealed by protect.
func unprotect(sealed []byte) ([]byte, error) {
	var in windows.DataBlob
	if len(sealed) == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: empty blob")
	}
	in.Size = uint32(len(sealed))
	in.Data = &sealed[0]

	var ent windows.DataBlob
	ent.Size = uint32(len(entropy))
	ent.Data = &entropy[0]

	var out windows.DataBlob
	flags := uint32(windows.CRYPTPROTECT_UI_FORBIDDEN)
	if err := windows.CryptUnprotectData(&in, nil, &ent, 0, nil, flags, &out); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	return copyAndFree(out)
}

// copyAndFree copies a DPAPI-allocated blob out of the LocalAlloc'd buffer and
// releases it. The copy is mandatory: the buffer must be freed with
// LocalFree, and the caller must not retain a pointer into it.
func copyAndFree(blob windows.DataBlob) ([]byte, error) {
	if blob.Data == nil || blob.Size == 0 {
		return nil, nil
	}
	buf := make([]byte, blob.Size)
	copy(buf, unsafe.Slice(blob.Data, blob.Size))
	if _, err := windows.LocalFree(windows.Handle(unsafe.Pointer(blob.Data))); err != nil {
		return nil, fmt.Errorf("LocalFree: %w", err)
	}
	return buf, nil
}

// writePrivate writes b to path so that no other user can read it.
//
// On Windows the file inherits the ACL of its directory, and the ACL is the
// thing that actually protects this file: a machine-scope DPAPI blob is
// readable by anything running as SYSTEM or an administrator, so restricting
// the directory to SYSTEM + Administrators is the security boundary, not the
// file mode. That ACL is set by the MSI (util:PermissionEx) and the agent does
// not attempt to build ACEs itself, because golang.org/x/sys/windows exposes no
// safe way to do so and a hand-rolled DACL is not worth the risk in an agent
// whose job is backups. See config.DataDirACLWarning for the startup check that
// catches a credentials directory the MSI never secured.
func writePrivate(path string, b []byte) error {
	return os.WriteFile(path, b, 0o600)
}

// Protection describes how this platform protects the blob, for the enrollment
// output.
func (s *Store) Protection() string {
	return "DPAPI, machine scope, plus the data directory ACL set by the installer"
}
