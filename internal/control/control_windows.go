package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/winacl"
)

// On Windows the control channel is the named pipe \\.\pipe\silkpool,
// as ADR 0008 has it:
//
//   - The agent creates it, as the first instance, owned by SYSTEM, with
//     pipeSDDL: SYSTEM has full control, and Authenticated Users only
//     read and write its data, read its attributes and its security
//     descriptor, and wait on it. So no account but SYSTEM can create an
//     instance, which FILE_CREATE_PIPE_INSTANCE, the same bit as
//     FILE_APPEND_DATA, would allow, or change its owner or DACL.
//     go-winio rejects remote clients, and makes the first instance with
//     FILE_CREATE, so the agent never joins a pipe that's already there.
//   - A client opens it with exactly clientAccess, and at the
//     identification level, so the server can read who it is but never
//     act as it. It accepts the pipe only if its own handle reads
//     pipeSDDL's owner and DACL exactly. Then it writes the preamble.
//   - The server reads the preamble, then reads the caller's token through
//     ImpersonateNamedPipeClient, which takes the security context of the
//     last message read. The caller is the token's user, an administrator
//     if the token holds BUILTIN\Administrators, enabled or, as UAC
//     filters an unelevated administrator's token, deny-only, and elevated
//     if the token is.
//
// What the client's check establishes is a SYSTEM-controlled endpoint,
// not the agent's binary, nor that the instance it reached is served by
// a SYSTEM process at that moment: it rests on no one privileged
// reassigning or relaxing the pipe's descriptor, and on SYSTEM's server
// or descriptor handles staying with SYSTEM. See ADR 0008.

func init() { Path = `\\.\pipe\silkpool` }

// needsElevation ends a refusal of a request that needs elevation.
const needsElevation = "needs an elevated token: run it from an administrator PowerShell"

// pipeSDDL is the pipe's security descriptor: owned by SYSTEM, with a
// protected DACL giving SYSTEM full control (FA, FILE_ALL_ACCESS) and
// Authenticated Users clientAccess.
const pipeSDDL = "O:SYD:P(A;;FA;;;SY)(A;;0x120083;;;AU)"

// clientAccess is all a client asks of the pipe: FILE_READ_DATA,
// FILE_WRITE_DATA, FILE_READ_ATTRIBUTES, READ_CONTROL and SYNCHRONIZE.
// CreateFile adds FILE_READ_ATTRIBUTES and SYNCHRONIZE to whatever's
// asked, so a DACL granting only data access would admit no one.
const clientAccess = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_READ_ATTRIBUTES |
	windows.READ_CONTROL | windows.SYNCHRONIZE

// fileAllAccess is FILE_ALL_ACCESS, SDDL's FA, which x/sys doesn't name:
// STANDARD_RIGHTS_REQUIRED, SYNCHRONIZE and every specific right.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// preamble is what a client writes once it has checked the pipe, before
// it reads the server's hello, so that the server impersonates a message
// the client sent.
const preamble = "silkpool control\n"

// preambleWait is how long the server waits for a client's preamble, and
// dialWait how long a client waits for a free instance of the pipe.
var (
	preambleWait = 10 * time.Second
	dialWait     = 10 * time.Second
)

// ListenPipe creates the pipe at name, as the first instance, and
// listens on it. It fails if a pipe of that name is already there,
// rather than join it.
func ListenPipe(name string) (net.Listener, error) {
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: pipeSDDL})
	if err != nil {
		return nil, fmt.Errorf("control: creating the pipe %s: %w", name, err)
	}
	return ln, nil
}

// A handled is a pipe's end, as go-winio makes it.
type handled interface{ Fd() uintptr }

// dial connects to the pipe at path and accepts it only if it's
// SYSTEM's, as pipeSDDL makes it, then writes the preamble. There's no
// uid on Windows: the pipe's owner stands in for serverUID.
func dial(ctx context.Context, path string, _ int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialWait)
	defer cancel()
	c, err := winio.DialPipeAccessImpLevel(ctx, path, clientAccess, winio.PipeImpLevelIdentification)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNotRunning
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("couldn't reach Silkpool's service at %s: every instance of it is busy", path)
	case err != nil:
		return nil, fmt.Errorf("couldn't reach Silkpool's service at %s: %w", path, err)
	}
	h, ok := c.(handled)
	if !ok {
		c.Close()
		return nil, fmt.Errorf("control: %T isn't a named pipe", c)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(h.Fd()), windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("couldn't check who serves %s: %w", path, err)
	}
	if err := checkPipe(sd); err != nil {
		c.Close()
		return nil, NotServer{Path: path, UID: -1, Detail: err.Error()}
	}
	if _, err := io.WriteString(c, preamble); err != nil {
		c.Close()
		return nil, fmt.Errorf("%w: %w", ErrDropped, err)
	}
	return c, nil
}

// pipeOwner and pipeEntries are pipeSDDL's owner and DACL, as a client
// must read them on its own handle.
var (
	pipeOwner   = winacl.WellKnown(windows.WinLocalSystemSid)
	pipeEntries = []winacl.Entry{
		{SID: winacl.WellKnown(windows.WinLocalSystemSid), Mask: fileAllAccess},
		{SID: winacl.WellKnown(windows.WinAuthenticatedUserSid), Mask: clientAccess},
	}
)

// checkPipe accepts sd only if its owner is SYSTEM and its DACL is
// present, protected, and exactly pipeSDDL's two allow entries.
func checkPipe(sd *windows.SECURITY_DESCRIPTOR) error {
	if err := winacl.Check(sd, pipeOwner, pipeEntries); err != nil {
		return fmt.Errorf("its pipe isn't SYSTEM's alone: %w", err)
	}
	return nil
}

// identify returns the account of the client at the other end of c,
// from its token: it reads the preamble, then the token, through
// impersonation at the client's level, which must let it identify the
// client. It fails on anything else, and never falls back to the
// agent's own token.
func identify(ctx context.Context, c net.Conn, _ func(int) (Account, error)) (Account, error) {
	h, ok := c.(handled)
	if !ok {
		return Account{}, fmt.Errorf("control: %T isn't a named pipe", c)
	}
	if err := readPreamble(ctx, c); err != nil {
		return Account{}, err
	}
	t, err := clientToken(windows.Handle(h.Fd()))
	if err != nil {
		return Account{}, err
	}
	defer t.Close()
	return accountOf(t)
}

// readPreamble reads the preamble from c, for at most preambleWait, or
// until ctx ends. It never closes c.
func readPreamble(ctx context.Context, c net.Conn) error {
	if err := c.SetReadDeadline(time.Now().Add(preambleWait)); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.SetReadDeadline(time.Now()) })
	defer stop()
	b := make([]byte, len(preamble))
	if _, err := io.ReadFull(c, b); err != nil {
		return fmt.Errorf("control: the client sent no preamble: %w", err)
	}
	if string(b) != preamble {
		return errors.New("control: the client opened with something other than the preamble")
	}
	return c.SetReadDeadline(time.Time{})
}

var procImpersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

// clientToken returns the token of the client of the pipe h, through
// impersonation on a thread locked to this goroutine, which it reverts
// before it unlocks it. A thread that can't revert still holds the
// client's identity, so it ends the agent rather than run anything else
// on it.
func clientToken(h windows.Handle) (windows.Token, error) {
	runtime.LockOSThread()
	r, _, callErr := procImpersonateNamedPipeClient.Call(uintptr(h))
	var t windows.Token
	var err error
	if r == 0 {
		err = fmt.Errorf("control: impersonating the client: %w", callErr)
	} else if err = windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &t); err != nil {
		err = fmt.Errorf("control: reading the client's token: %w", err)
	}
	if revertErr := windows.RevertToSelf(); revertErr != nil {
		panic("control: couldn't stop impersonating a client of the control pipe: " + revertErr.Error())
	}
	runtime.UnlockOSThread()
	if err != nil {
		if t != 0 {
			t.Close()
		}
		return 0, err
	}
	return t, nil
}

// accountOf returns the account t is, failing unless each of its parts
// can be read.
func accountOf(t windows.Token) (Account, error) {
	var level uint32
	if err := tokenInfo(t, windows.TokenImpersonationLevel, &level); err != nil {
		return Account{}, fmt.Errorf("control: reading the client's impersonation level: %w", err)
	}
	if level < windows.SecurityIdentification {
		return Account{}, fmt.Errorf("control: the client's token is at impersonation level %d, which doesn't identify it", level)
	}
	user, err := t.GetTokenUser()
	if err != nil {
		return Account{}, fmt.Errorf("control: reading the client's user: %w", err)
	}
	groups, err := t.GetTokenGroups()
	if err != nil {
		return Account{}, fmt.Errorf("control: reading the client's groups: %w", err)
	}
	var elevation uint32
	if err := tokenInfo(t, windows.TokenElevation, &elevation); err != nil {
		return Account{}, fmt.Errorf("control: reading whether the client is elevated: %w", err)
	}
	admins := winacl.WellKnown(windows.WinBuiltinAdministratorsSid)
	admin := false
	for _, g := range groups.AllGroups() {
		if g.Sid.Equals(admins) && g.Attributes&(windows.SE_GROUP_ENABLED|windows.SE_GROUP_USE_FOR_DENY_ONLY) != 0 {
			admin = true
		}
	}
	return Account{Name: accountName(user.User.Sid), Admin: admin, Elevated: elevation != 0}, nil
}

// tokenInfo reads one uint32 of t's information.
func tokenInfo(t windows.Token, class uint32, v *uint32) error {
	var n uint32
	return windows.GetTokenInformation(t, class, (*byte)(unsafe.Pointer(v)), uint32(unsafe.Sizeof(*v)), &n)
}

// accountName names sid's account: by its name alone if it's this
// machine's or Windows's own, else as DOMAIN\name, or by the SID itself if
// it has no name.
func accountName(sid *windows.SID) string {
	name, domain, _, err := sid.LookupAccount("")
	if err != nil || name == "" {
		return sid.String()
	}
	// Windows's names are the same in any case: an account's lookup can
	// give its machine's name in lower case, where the computer name is in
	// upper case.
	host, _ := windows.ComputerName()
	for _, local := range []string{"", host, "NT AUTHORITY", "BUILTIN"} {
		if strings.EqualFold(domain, local) {
			return name
		}
	}
	return domain + `\` + name
}
