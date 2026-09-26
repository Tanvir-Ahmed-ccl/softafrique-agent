//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// currentUserName returns the account the agent runs as as "DOMAIN\user", which
// is the form icacls expects in a /grant:r.
func currentUserName() string {
	sid, err := processUserSID()
	if err != nil {
		return "Administrators"
	}
	who, err := accountFor(sid)
	if err != nil {
		return "Administrators"
	}
	return who
}

func processUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func accountFor(sid *windows.SID) (string, error) {
	acct, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return "", err
	}
	if domain == "" || domain == "." {
		return acct, nil
	}
	return domain + `\` + acct, nil
}

// tokenFileReaders lists the accounts that can read a file, read from its real
// DACL.
//
// A mode-bit check would be useless here: Go synthesises a mode on Windows that
// says nothing about the ACL, and the enrollment token is a one-time secret that
// lets whoever holds it register a machine as this customer. So the DACL is
// walked directly, and deny ACEs are honoured because denying Everyone and then
// granting the installer is the normal way to write these files.
func tokenFileReaders(path string) ([]string, error) {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return nil, fmt.Errorf("reading the permissions of %s: %w", path, err)
	}

	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		// A missing or NULL DACL grants everyone full access. There is no way to
		// make that safe, so name the problem instead of quietly allowing it.
		return []string{"everyone, because " + path + " has no access control list"}, nil
	}

	denied := map[string]bool{}
	var allowed []string

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			continue
		}
		// ACCESS_ALLOWED_ACE and ACCESS_DENIED_ACE share a layout, so the deny
		// entries read correctly through the allow type; only the type differs.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		who, err := accountFor(sid)
		if err != nil {
			// An unresolvable SID is a trustee this build cannot name, so it
			// cannot be recognised as the installer either. Treat it as untrusted
			// and let the operator see it.
			who = "an account this build cannot resolve"
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			denied[who] = true
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if aceAllowsRead(ace.Mask) {
				allowed = append(allowed, who)
			}
		}
	}

	// Windows evaluates deny ACEs before allow ACEs, so drop anyone denied
	// anywhere in the list.
	out := allowed[:0]
	for _, who := range allowed {
		if !denied[who] {
			out = append(out, who)
		}
	}
	return out, nil
}

// aceAllowsRead reports whether an access mask grants any form of read.
func aceAllowsRead(mask windows.ACCESS_MASK) bool {
	const readBits = windows.FILE_GENERIC_READ | windows.GENERIC_READ |
		windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA
	return mask&readBits != 0
}
