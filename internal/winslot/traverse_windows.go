package winslot

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")

// WithoutTraverseBypass returns a copy of the service account's token t
// without SeChangeNotifyPrivilege, which the caller closes: the token
// every job runs as, and the exposure check acts as (ADR 0020). Every
// token has that privilege, "Bypass traverse checking", so a file whose
// own DACL lets it be read can be opened by its full path though a
// folder above it denies traversing. Without it, a folder the read guard
// denies the account covers all that's below it.
//
// It has no restricting SIDs, so it isn't a restricted token, which
// schannel would refuse; and it needs no privilege: a copy of a
// process's own token with a privilege taken away is one
// CreateProcessAsUser takes without SeAssignPrimaryTokenPrivilege.
func WithoutTraverseBypass(t windows.Token) (windows.Token, error) {
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeChangeNotifyPrivilege"), &luid); err != nil {
		return 0, fmt.Errorf("looking up SeChangeNotifyPrivilege: %w", err)
	}
	gone := []windows.LUIDAndAttributes{{Luid: luid}}
	var r windows.Token
	if ok, _, err := procCreateRestrictedToken.Call(uintptr(t), 0, 0, 0,
		uintptr(len(gone)), uintptr(unsafe.Pointer(&gone[0])), 0, 0, uintptr(unsafe.Pointer(&r))); ok == 0 {
		return 0, fmt.Errorf("taking bypassing traverse checking from the token: %w", err)
	}
	return r, nil
}
