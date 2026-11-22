package runnerinstall

import (
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/meschoening/silkpool/internal/winacl"
)

// Everything Installs makes on Windows gets a descriptor of its own:
// owned by the agent, with a DACL protected from what's above it, so
// that nothing set on a folder around it, such as a slot's, reaches it. A
// hardlink has no descriptor of its own, so each of an install's files
// keeps its descriptor in every runner's root that links it.
//
// The agent is the account the process runs as: LocalSystem, as its
// service runs it. The descriptors name it rather than SYSTEM, as Linux's
// modes name the owner rather than root, so that the windows lane, which
// runs as a standard account, makes them as the agent would.
type sids struct {
	// agent and account are the agent's SID and the service account's.
	agent, account string
}

// sids returns the SIDs the descriptors name, refusing an Account that
// isn't a SID.
func (i Installs) sids() (sids, error) {
	account, err := windows.StringToSid(i.Account)
	if err != nil {
		return sids{}, fmt.Errorf("%q isn't a SID for Silkpool's service account", i.Account)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return sids{}, fmt.Errorf("finding the agent's account: %w", err)
	}
	return sids{agent: u.User.Sid.String(), account: account.String()}, nil
}

// private is the installs' folder's descriptor, and a runner's root's
// until it's complete: the agent's alone, and so is everything made in
// it that doesn't get a descriptor of its own.
func (s sids) private() (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%[1]sD:P(A;OICI;FA;;;%[1]s)", s.agent))
}

// isPrivate reports whether what's at path already has the private
// descriptor: owned by the agent, with a protected DACL of exactly its
// one entry.
func (s sids) isPrivate(path string) bool {
	agent, err := windows.StringToSid(s.agent)
	if err != nil {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	return winacl.Check(sd, agent, []winacl.Entry{
		{SID: agent, Mask: fileAllAccess, Flags: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE},
	}) == nil
}

// fileAllAccess is FILE_ALL_ACCESS, SDDL's FA, which x/sys doesn't name.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// readable is the descriptor of each of an install's files, of a copy
// of one, and of each folder in a runner's root: the account may read
// and run it, or list it and pass through it, and nothing more. 0x1200a9
// is FILE_GENERIC_READ and FILE_EXECUTE.
func (s sids) readable() (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%[1]sD:P(A;;FA;;;%[1]s)(A;;0x1200a9;;;%[2]s)", s.agent, s.account))
}

// root is a runner's root folder's descriptor, once it's complete: the
// account's, and so is everything the runner makes in it.
func (s sids) root() (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString(fmt.Sprintf("O:%[1]sD:P(A;OICI;FA;;;%[1]s)(A;OICI;FA;;;%[2]s)", s.agent, s.account))
}

// apply gives what's at path the owner and DACL of sd.
func apply(path string, sd *windows.SECURITY_DESCRIPTOR) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
	if err != nil {
		return &os.PathError{Op: "secure", Path: path, Err: err}
	}
	return nil
}

// attributes returns sd as the attributes a new file or folder is made
// with, so it never exists without it.
func attributes(sd *windows.SECURITY_DESCRIPTOR) *windows.SecurityAttributes {
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa
}

// mkdir makes the folder path, which mustn't exist yet, with sd.
func mkdir(path string, sd *windows.SECURITY_DESCRIPTOR) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(p, attributes(sd)); err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// copyFile copies the file src to dst, which mustn't exist yet, with
// sd.
func copyFile(src, dst string, sd *windows.SECURITY_DESCRIPTOR) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	p, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, attributes(sd), windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.PathError{Op: "create", Path: dst, Err: err}
	}
	out := os.NewFile(uintptr(h), dst)
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
