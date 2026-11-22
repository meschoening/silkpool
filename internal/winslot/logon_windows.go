package winslot

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	secur32                            = windows.NewLazySystemDLL("secur32.dll")
	procLsaConnectUntrusted            = secur32.NewProc("LsaConnectUntrusted")
	procLsaDeregisterLogonProcess      = secur32.NewProc("LsaDeregisterLogonProcess")
	procLsaLookupAuthenticationPackage = secur32.NewProc("LsaLookupAuthenticationPackage")
	procLsaLogonUser                   = secur32.NewProc("LsaLogonUser")
	procLsaFreeReturnBuffer            = secur32.NewProc("LsaFreeReturnBuffer")

	advapi32                    = windows.NewLazySystemDLL("advapi32.dll")
	procAllocateLocallyUniqueId = advapi32.NewProc("AllocateLocallyUniqueId")

	userenv               = windows.NewLazySystemDLL("userenv.dll")
	procLoadUserProfileW  = userenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile = userenv.NewProc("UnloadUserProfile")
)

// The logon's constants: logon type Batch, the only one S4U allows
// besides Network, and MSV1_0_LOGON_SUBMIT_TYPE's MsV1_0S4ULogon, as
// "Verify passwordless slot logon on Windows" used them.
const (
	logonBatch  = 4
	msv1_0S4U   = 12
	piNoUI      = 1
	tokenSource = "Silkpool"
)

// msv1_0S4ULogon is MSV1_0_S4U_LOGON. Its two strings follow it in the
// same allocation.
type msv1_0S4ULogon struct {
	MessageType       uint32
	Flags             uint32
	UserPrincipalName windows.NTUnicodeString
	DomainName        windows.NTUnicodeString
}

// tokenSourceInfo is TOKEN_SOURCE.
type tokenSourceInfo struct {
	SourceName       [8]byte
	SourceIdentifier windows.LUID
}

// quotaLimits is QUOTA_LIMITS.
type quotaLimits struct {
	PagedPoolLimit, NonPagedPoolLimit, MinimumWorkingSetSize, MaximumWorkingSetSize, PagefileLimit uintptr
	TimeLimit                                                                                      int64
}

// profileInfo is PROFILEINFOW.
type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

// s4u logs on as the spec's account with LsaLogonUser, MSV1_0_S4U_LOGON
// and logon type Batch, with "Silkpool" as the token's source, so no
// password is needed (LogonToken). It needs SeTcbPrivilege, which
// LocalSystem holds. It refuses a token whose user isn't the spec's SID,
// then loads the account's profile, which every slot's host shares.
func s4u(s Spec) (session, error) {
	token, err := LogonToken(s.Account, s.SID)
	if err != nil {
		return session{}, err
	}
	pi := profileInfo{Flags: piNoUI, UserName: windows.StringToUTF16Ptr(s.Account)}
	pi.Size = uint32(unsafe.Sizeof(pi))
	if r, _, err := procLoadUserProfileW.Call(uintptr(token), uintptr(unsafe.Pointer(&pi))); r == 0 {
		token.Close()
		return session{}, fmt.Errorf("loading %s's profile: %w", s.Account, err)
	}
	end := func() error {
		var errs []error
		if r, _, err := procUnloadUserProfile.Call(uintptr(token), uintptr(pi.Profile)); r == 0 {
			errs = append(errs, fmt.Errorf("unloading %s's profile: %w", s.Account, err))
		}
		errs = append(errs, token.Close())
		return errors.Join(errs...)
	}
	return session{token: token, end: end}, nil
}

// LogonToken logs on as account, as a slot host logs on as it for its
// listener: S4U, logon type Batch, with "Silkpool" as the token's
// source. It refuses a token whose user isn't sid, the SID Silkpool
// recorded for the account, and returns the token, which the caller
// closes. It doesn't load the account's profile, which only a listener
// needs: the exposure check asks only what the token may open (ADR
// 0018). It needs SeTcbPrivilege, which LocalSystem holds.
func LogonToken(account, sid string) (windows.Token, error) {
	want, err := windows.StringToSid(sid)
	if err != nil {
		return 0, fmt.Errorf("the account's SID %q: %w", sid, err)
	}
	domain, err := windows.ComputerName()
	if err != nil {
		return 0, fmt.Errorf("reading the computer's name: %w", err)
	}
	token, err := lsaLogon(account, domain)
	if err != nil {
		return 0, err
	}
	u, err := token.GetTokenUser()
	if err != nil {
		token.Close()
		return 0, fmt.Errorf("reading the logon's user: %w", err)
	}
	if !windows.EqualSid(u.User.Sid, want) {
		token.Close()
		return 0, fmt.Errorf("the logon as %s is %s, not the account recorded, %s", account, u.User.Sid, sid)
	}
	return token, nil
}

// lsaLogon returns a primary token for user on domain, the computer for
// a local account, by an untrusted connection to the LSA, which is
// enough for S4U.
func lsaLogon(user, domain string) (windows.Token, error) {
	var lsa windows.Handle
	if r, _, _ := procLsaConnectUntrusted.Call(uintptr(unsafe.Pointer(&lsa))); r != 0 {
		return 0, fmt.Errorf("connecting to the LSA: %w", windows.NTStatus(r).Errno())
	}
	defer procLsaDeregisterLogonProcess.Call(uintptr(lsa))
	msv, err := windows.NewNTString("MICROSOFT_AUTHENTICATION_PACKAGE_V1_0")
	if err != nil {
		return 0, err
	}
	var pkg uint32
	if r, _, _ := procLsaLookupAuthenticationPackage.Call(uintptr(lsa), uintptr(unsafe.Pointer(msv)), uintptr(unsafe.Pointer(&pkg))); r != 0 {
		return 0, fmt.Errorf("finding MSV1_0: %w", windows.NTStatus(r).Errno())
	}

	u, err := windows.UTF16FromString(user)
	if err != nil {
		return 0, err
	}
	d, err := windows.UTF16FromString(domain)
	if err != nil {
		return 0, err
	}
	u, d = u[:len(u)-1], d[:len(d)-1]
	head := unsafe.Sizeof(msv1_0S4ULogon{})
	size := head + uintptr(len(u)+len(d))*2
	// The strings follow the struct in one buffer, which LsaLogonUser
	// reads in place. Go's heap doesn't move, and the buffer is kept
	// alive past the call.
	words := make([]uint64, (size+7)/8)
	buf := unsafe.Pointer(&words[0])
	info := (*msv1_0S4ULogon)(buf)
	info.MessageType = msv1_0S4U
	up := (*uint16)(unsafe.Add(buf, head))
	copy(unsafe.Slice(up, len(u)), u)
	info.UserPrincipalName = windows.NTUnicodeString{Length: uint16(len(u) * 2), MaximumLength: uint16(len(u) * 2), Buffer: up}
	dp := (*uint16)(unsafe.Add(buf, head+uintptr(len(u))*2))
	copy(unsafe.Slice(dp, len(d)), d)
	info.DomainName = windows.NTUnicodeString{Length: uint16(len(d) * 2), MaximumLength: uint16(len(d) * 2), Buffer: dp}

	origin, err := windows.NewNTString(tokenSource)
	if err != nil {
		return 0, err
	}
	var src tokenSourceInfo
	copy(src.SourceName[:], tokenSource)
	if r, _, err := procAllocateLocallyUniqueId.Call(uintptr(unsafe.Pointer(&src.SourceIdentifier))); r == 0 {
		return 0, fmt.Errorf("making the token source's ID: %w", err)
	}
	var (
		profile    uintptr
		profileLen uint32
		logonID    windows.LUID
		token      windows.Token
		quotas     quotaLimits
		sub        windows.NTStatus
	)
	r, _, _ := procLsaLogonUser.Call(
		uintptr(lsa),
		uintptr(unsafe.Pointer(origin)),
		logonBatch,
		uintptr(pkg),
		uintptr(buf),
		size,
		0,
		uintptr(unsafe.Pointer(&src)),
		uintptr(unsafe.Pointer(&profile)),
		uintptr(unsafe.Pointer(&profileLen)),
		uintptr(unsafe.Pointer(&logonID)),
		uintptr(unsafe.Pointer(&token)),
		uintptr(unsafe.Pointer(&quotas)),
		uintptr(unsafe.Pointer(&sub)),
	)
	runtime.KeepAlive(words)
	if profile != 0 {
		procLsaFreeReturnBuffer.Call(profile)
	}
	if r != 0 {
		if sub != 0 {
			return 0, fmt.Errorf("S4U logon (substatus %#08x): %w", uint32(sub), windows.NTStatus(r).Errno())
		}
		return 0, fmt.Errorf("S4U logon: %w", windows.NTStatus(r).Errno())
	}
	return token, nil
}
