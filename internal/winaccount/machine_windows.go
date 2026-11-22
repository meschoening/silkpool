package winaccount

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Local is this machine's local accounts, through the Net, LSA, registry
// and profile APIs. Reading them works as any account; changing them
// needs an administrator, and the agent runs as LocalSystem.
type Local struct{}

var _ Machine = Local{}

var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserAdd              = netapi32.NewProc("NetUserAdd")
	procNetUserSetInfo          = netapi32.NewProc("NetUserSetInfo")
	procNetUserDel              = netapi32.NewProc("NetUserDel")
	procNetUserGetLocalGroups   = netapi32.NewProc("NetUserGetLocalGroups")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")

	advapi32                      = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy             = advapi32.NewProc("LsaOpenPolicy")
	procLsaClose                  = advapi32.NewProc("LsaClose")
	procLsaFreeMemory             = advapi32.NewProc("LsaFreeMemory")
	procLsaNtStatusToWinError     = advapi32.NewProc("LsaNtStatusToWinError")
	procLsaEnumerateAccountRights = advapi32.NewProc("LsaEnumerateAccountRights")
	procLsaAddAccountRights       = advapi32.NewProc("LsaAddAccountRights")
	procLsaRemoveAccountRights    = advapi32.NewProc("LsaRemoveAccountRights")

	userenv           = windows.NewLazySystemDLL("userenv.dll")
	procDeleteProfile = userenv.NewProc("DeleteProfileW")
)

// The Net API's constants Silkpool uses, from lmaccess.h and lmerr.h.
const (
	nerrUserNotFound     = 2221
	errorMemberInAlias   = 1378
	userPrivUser         = 1
	ufScript             = 0x0001
	ufAccountDisable     = 0x0002
	ufLockout            = 0x0010
	timeqForever         = 0xFFFFFFFF
	maxPreferredLength   = 0xFFFFFFFF
	levelComment         = 1007
	levelFlags           = 1008
	levelAcctExpires     = 1017
	policyCreateAccount  = 0x00000010
	policyLookupNames    = 0x00000800
	statusObjectNotFound = 0xC0000034
)

// userInfo1 is USER_INFO_1.
type userInfo1 struct {
	Name, Password    *uint16
	PasswordAge, Priv uint32
	HomeDir, Comment  *uint16
	Flags             uint32
	ScriptPath        *uint16
}

// userInfo4 is USER_INFO_4.
type userInfo4 struct {
	Name, Password                                 *uint16
	PasswordAge, Priv                              uint32
	HomeDir, Comment                               *uint16
	Flags                                          uint32
	ScriptPath                                     *uint16
	AuthFlags                                      uint32
	FullName, UsrComment, Parms, Workstations      *uint16
	LastLogon, LastLogoff, AcctExpires, MaxStorage uint32
	UnitsPerWeek                                   uint32
	LogonHours                                     *byte
	BadPwCount, NumLogons                          uint32
	LogonServer                                    *uint16
	CountryCode, CodePage                          uint32
	UserSID                                        *windows.SID
	PrimaryGroupID                                 uint32
	Profile, HomeDirDrive                          *uint16
	PasswordExpired                                uint32
}

// netErr is a Net API status as an error, or nil.
func netErr(r uintptr) error {
	if r == 0 {
		return nil
	}
	return windows.Errno(r)
}

func (Local) Lookup(name string) (User, bool, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return User{}, false, err
	}
	var buf *byte
	err = windows.NetUserGetInfo(nil, n, 4, &buf)
	if errors.Is(err, windows.Errno(nerrUserNotFound)) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("can't read the account %s: %w", name, err)
	}
	defer windows.NetApiBufferFree(buf)
	i := (*userInfo4)(unsafe.Pointer(buf))
	if i.UserSID == nil {
		return User{}, false, fmt.Errorf("the account %s has no SID", name)
	}
	return User{
		SID:       i.UserSID.String(),
		Comment:   windows.UTF16PtrToString(i.Comment),
		Disabled:  i.Flags&ufAccountDisable != 0,
		LockedOut: i.Flags&ufLockout != 0,
		Expires:   i.AcctExpires != timeqForever,
	}, true, nil
}

func (Local) Create(name, comment, password string) error {
	n, err1 := windows.UTF16PtrFromString(name)
	p, err2 := windows.UTF16PtrFromString(password)
	c, err3 := windows.UTF16PtrFromString(comment)
	if err := errors.Join(err1, err2, err3); err != nil {
		return err
	}
	info := userInfo1{Name: n, Password: p, Priv: userPrivUser, Comment: c, Flags: ufScript}
	r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&info)), 0)
	// The password's copy in UTF-16 is the one Silkpool can clear.
	clear(unsafe.Slice(p, len(password)))
	runtime.KeepAlive(&info)
	return netErr(r)
}

// setInfo sets one field of name's USER_INFO through info, a USER_INFO_
// 10xx of level.
func setInfo(name string, level uint32, info unsafe.Pointer) error {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(n)), uintptr(level), uintptr(info), 0)
	return netErr(r)
}

func (Local) SetComment(name, comment string) error {
	c, err := windows.UTF16PtrFromString(comment)
	if err != nil {
		return err
	}
	info := struct{ Comment *uint16 }{c}
	return setInfo(name, levelComment, unsafe.Pointer(&info))
}

// flags reads name's flags.
func flags(name string) (uint32, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	var buf *byte
	if err := windows.NetUserGetInfo(nil, n, 1, &buf); err != nil {
		return 0, err
	}
	defer windows.NetApiBufferFree(buf)
	return (*userInfo1)(unsafe.Pointer(buf)).Flags, nil
}

// setFlags clears off in name's flags, and sets on.
func setFlags(name string, off, on uint32) error {
	f, err := flags(name)
	if err != nil {
		return err
	}
	info := struct{ Flags uint32 }{f&^off | on}
	return setInfo(name, levelFlags, unsafe.Pointer(&info))
}

func (Local) Enable(name string) error  { return setFlags(name, ufAccountDisable|ufLockout, 0) }
func (Local) Disable(name string) error { return setFlags(name, 0, ufAccountDisable) }

func (Local) NeverExpire(name string) error {
	info := struct{ AcctExpires uint32 }{timeqForever}
	return setInfo(name, levelAcctExpires, unsafe.Pointer(&info))
}

func (Local) Groups(name string) ([]Group, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var buf *byte
	var read, total uint32
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(n)), 0, 0,
		uintptr(unsafe.Pointer(&buf)), maxPreferredLength, uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if buf != nil {
		defer windows.NetApiBufferFree(buf)
	}
	if err := netErr(r); err != nil {
		return nil, err
	}
	if read != total {
		return nil, fmt.Errorf("read %d of %s's %d groups", read, name, total)
	}
	var groups []Group
	for _, p := range unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read) {
		g := windows.UTF16PtrToString(p)
		sid, _, _, err := windows.LookupSID("", g)
		if err != nil {
			return nil, fmt.Errorf("can't find the group %s: %w", g, err)
		}
		groups = append(groups, Group{Name: g, SID: sid.String()})
	}
	return groups, nil
}

func (Local) AddToUsers(sid string) error {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return err
	}
	u, err := windows.StringToSid(UsersSID)
	if err != nil {
		return err
	}
	// Its name is the machine's language's.
	name, _, _, err := u.LookupAccount("")
	if err != nil {
		return err
	}
	g, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	member := struct{ SID *windows.SID }{s}
	r, _, _ := procNetLocalGroupAddMembers.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&member)), 1)
	runtime.KeepAlive(s)
	if r == errorMemberInAlias {
		return nil
	}
	return netErr(r)
}

// lsaString is LSA_UNICODE_STRING.
type lsaString struct {
	Length, MaximumLength uint16
	Buffer                *uint16
}

func newLSAString(s string) (lsaString, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return lsaString{}, err
	}
	return lsaString{Length: uint16(2 * (len(u) - 1)), MaximumLength: uint16(2 * len(u)), Buffer: &u[0]}, nil
}

// lsaErr is an NTSTATUS as an error, or nil.
func lsaErr(status uintptr) error {
	if status == 0 {
		return nil
	}
	r, _, _ := procLsaNtStatusToWinError.Call(status)
	return windows.Errno(r)
}

// policy opens the local LSA policy with access, and passes it to use.
func policy(access uint32, use func(h uintptr) error) error {
	// LSA_OBJECT_ATTRIBUTES, all zero, as LsaOpenPolicy asks.
	var attrs struct {
		Length                   uint32
		RootDirectory            uintptr
		ObjectName               uintptr
		Attributes               uint32
		SecurityDescriptor       uintptr
		SecurityQualityOfService uintptr
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h uintptr
	r, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)), uintptr(access), uintptr(unsafe.Pointer(&h)))
	if err := lsaErr(r); err != nil {
		return fmt.Errorf("can't open the LSA policy: %w", err)
	}
	defer procLsaClose.Call(h)
	return use(h)
}

func (Local) Rights(sid string) ([]string, error) {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return nil, err
	}
	var rights []string
	err = policy(policyLookupNames, func(h uintptr) error {
		var buf *lsaString
		var n uint32
		r, _, _ := procLsaEnumerateAccountRights.Call(h, uintptr(unsafe.Pointer(s)),
			uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
		if r == statusObjectNotFound {
			// It holds none.
			return nil
		}
		if err := lsaErr(r); err != nil {
			return err
		}
		defer procLsaFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
		for _, l := range unsafe.Slice(buf, n) {
			rights = append(rights, windows.UTF16ToString(unsafe.Slice(l.Buffer, l.Length/2)))
		}
		return nil
	})
	runtime.KeepAlive(s)
	return rights, err
}

// lsaStrings are rights as LSA_UNICODE_STRINGs.
func lsaStrings(rights []string) ([]lsaString, error) {
	l := make([]lsaString, len(rights))
	for i, r := range rights {
		var err error
		if l[i], err = newLSAString(r); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (Local) AddRights(sid string, rights []string) error {
	return changeRights(sid, rights, false, false)
}

func (Local) RemoveRights(sid string, rights []string) error {
	return changeRights(sid, rights, true, false)
}

// RemoveAllRights removes every right sid holds, and the LSA's record of
// it.
func (Local) RemoveAllRights(sid string) error {
	return changeRights(sid, nil, true, true)
}

func changeRights(sid string, rights []string, remove, all bool) error {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return err
	}
	l, err := lsaStrings(rights)
	if err != nil {
		return err
	}
	var p uintptr
	if len(l) > 0 {
		p = uintptr(unsafe.Pointer(&l[0]))
	}
	err = policy(policyLookupNames|policyCreateAccount, func(h uintptr) error {
		var r uintptr
		if remove {
			var a uintptr
			if all {
				a = 1
			}
			r, _, _ = procLsaRemoveAccountRights.Call(h, uintptr(unsafe.Pointer(s)), a, p, uintptr(len(l)))
			if r == statusObjectNotFound {
				return nil
			}
		} else {
			r, _, _ = procLsaAddAccountRights.Call(h, uintptr(unsafe.Pointer(s)), p, uintptr(len(l)))
		}
		return lsaErr(r)
	})
	runtime.KeepAlive(s)
	runtime.KeepAlive(l)
	return err
}

// userList is the registry key whose DWORD values of 0 hide the accounts
// they name from the sign-in screen.
const userList = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList`

func (Local) Hidden(name string) (bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, userList, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer k.Close()
	v, typ, err := k.GetIntegerValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, registry.ErrUnexpectedType) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return typ == registry.DWORD && v == 0, nil
}

func (Local) Hide(name string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, userList, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue(name, 0)
}

func (Local) Unhide(name string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, userList, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// Processes counts the processes whose token's user is sid. As
// LocalSystem, the agent can read every process's token but a protected
// process's, and the account can't start a protected process, so those
// it passes over. As another account, it counts only the processes that
// account can read.
func (Local) Processes(sid string) (int, error) {
	want, err := windows.StringToSid(sid)
	if err != nil {
		return 0, err
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	n := 0
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if runsAs(e.ProcessID, want) {
			n++
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, err
	}
	return n, nil
}

// runsAs reports whether process pid runs as sid, as far as its token
// can be read: a process that has ended, or whose token can't be opened,
// doesn't.
func runsAs(pid uint32, sid *windows.SID) bool {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(p)
	var t windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &t); err != nil {
		return false
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	return err == nil && u.User.Sid.Equals(sid)
}

func (Local) DeleteProfile(sid string) error {
	s, err := windows.UTF16PtrFromString(sid)
	if err != nil {
		return err
	}
	r, _, err := procDeleteProfile.Call(uintptr(unsafe.Pointer(s)), 0, 0)
	if r != 0 || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	return err
}

func (Local) Delete(name string) error {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(n)))
	if r == nerrUserNotFound {
		return nil
	}
	return netErr(r)
}
