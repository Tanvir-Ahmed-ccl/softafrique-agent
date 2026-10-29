//go:build windows

package config

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dataDirACLWarning reports whether accounts other than SYSTEM and
// Administrators can read the data directory. The credentials blob lives there,
// and machine-scope DPAPI does not protect it from anything running as SYSTEM or
// an administrator. The directory ACL is the boundary, so the agent has to check
// the ACL it actually got rather than trust that the installer's grant worked.
func dataDirACLWarning(dir string) string {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Nothing has been written yet, so there is no blob here to expose. The
		// next run creates the directory and checks it then; warning now would
		// make a fresh, correctly installed service log a false alarm forever.
		return ""
	}
	readers, err := dataDirReaders(dir)
	if err != nil {
		return fmt.Sprintf("could not verify the access control list on %s: %v; "+
			"confirm that only SYSTEM and Administrators can read it", dir, err)
	}
	if len(readers) == 0 {
		return ""
	}
	return fmt.Sprintf("data directory %s is readable by %s; restrict it to SYSTEM and Administrators with: "+
		`icacls "%s" /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"`,
		dir, strings.Join(readers, ", "), dir)
}

// dataDirReaders returns the trustees that an allow ACE lets read dir, after
// removing anyone a deny ACE covers. It is a direct DACL walk because file mode
// bits on Windows are synthesised by Go and say nothing about the ACL.
func dataDirReaders(dir string) ([]string, error) {
	sd, err := windows.GetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	if dacl == nil {
		return []string{"everyone, because the directory has no access control list"}, nil
	}

	denied := map[string]bool{}
	type reader struct {
		sid  string
		name string
	}
	var allowed []reader
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		key := sid.String()
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			denied[key] = true
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if !aceAllowsRead(ace.Mask) || trustedDataDirReader(key) {
				continue
			}
			allowed = append(allowed, reader{sid: key, name: dataDirAccountName(sid)})
		}
	}

	readers := make([]string, 0, len(allowed))
	for _, r := range allowed {
		if !denied[r.sid] {
			readers = append(readers, r.name)
		}
	}
	return readers, nil
}

// trustedDataDirReader reports whether a SID is one of the two accounts the
// installer is supposed to leave with access.
func trustedDataDirReader(sid string) bool {
	switch strings.ToUpper(sid) {
	case "S-1-5-18", "S-1-5-32-544":
		return true
	default:
		return false
	}
}

func dataDirAccountName(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil || account == "" {
		return sid.String()
	}
	if domain == "" || domain == "." {
		return account
	}
	return domain + `\` + account
}

func aceAllowsRead(mask windows.ACCESS_MASK) bool {
	const readBits = windows.FILE_GENERIC_READ | windows.GENERIC_READ |
		windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA
	return mask&readBits != 0
}
