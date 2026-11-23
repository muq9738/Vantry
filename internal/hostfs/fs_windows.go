//go:build windows

package hostfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/muq9738/Vantry/internal/hostprocess"
	"golang.org/x/sys/windows"
)

func privateAttributes() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func openHandle(path string, access, disposition, flags uint32, sa *windows.SecurityAttributes) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// Match Unix open handles: managed cleanup can unlink metadata while its
	// owner still holds the lock. Identity checks use the opened handle.
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, sa, disposition, flags|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return h, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := checkHandle(h); err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

func checkHandle(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("reparse points are not allowed")
	}
	kind, err := windows.GetFileType(h)
	if err != nil {
		return err
	}
	if kind != windows.FILE_TYPE_DISK {
		return errors.New("only disk files and directories are allowed")
	}
	return nil
}

func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	access := uint32(windows.GENERIC_READ)
	if flag&os.O_RDWR != 0 {
		access |= windows.GENERIC_WRITE
	} else if flag&os.O_WRONLY != 0 {
		access = windows.GENERIC_WRITE
	}
	disposition := uint32(windows.OPEN_EXISTING)
	var sa *windows.SecurityAttributes
	var err error
	if flag&os.O_CREATE != 0 {
		disposition = windows.OPEN_ALWAYS
		sa, err = privateAttributes()
		if err != nil {
			return nil, err
		}
		if flag&os.O_EXCL != 0 {
			disposition = windows.CREATE_NEW
		}
	}
	if flag&(os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errors.New("unsupported no-follow open flags")
	}
	h, err := openHandle(path, access, disposition, 0, sa)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// OpenRootNoFollow walks relative to directory handles. No path component can
// become a junction or symlink between validation and opening the final file.
func OpenRootNoFollow(root *os.Root, path string) (*os.File, error) {
	if !filepath.IsLocal(path) || strings.Contains(path, ":") {
		return nil, errors.New("file path must stay inside its root")
	}
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i, part := range parts {
		name, err := windows.NewNTUnicodeString(part)
		if err != nil {
			return nil, err
		}
		attrs := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(parent.Fd()), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
		options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
		if i < len(parts)-1 {
			options |= windows.FILE_DIRECTORY_FILE
		} else {
			options |= windows.FILE_NON_DIRECTORY_FILE
		}
		var h windows.Handle
		err = windows.NtCreateFile(&h, windows.FILE_GENERIC_READ, &attrs, &windows.IO_STATUS_BLOCK{}, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0)
		if err != nil {
			if status, ok := err.(windows.NTStatus); ok {
				err = status.Errno()
			}
			return nil, &os.PathError{Op: "openat", Path: path, Err: err}
		}
		if err := checkHandle(h); err != nil {
			windows.CloseHandle(h)
			return nil, err
		}
		next := os.NewFile(uintptr(h), part)
		if i == len(parts)-1 {
			return next, nil
		}
		_ = parent.Close()
		parent = next
	}
	return nil, errors.New("empty file path")
}

func EnsurePrivateDir(path string) error {
	if path == "" {
		return errors.New("private directory path is empty")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
			if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
				return err
			}
		}
		sa, err := privateAttributes()
		if err != nil {
			return err
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		if err := windows.CreateDirectory(name, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
	} else if err != nil {
		return err
	}
	return CheckPrivateDir(path, true)
}

func CheckPrivateDir(path string, owner bool) error {
	h, err := openHandle(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("not a directory: %s", path)
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	actual, _, err := sd.Owner()
	if err != nil || actual == nil || !actual.Equals(user.User.Sid) {
		return fmt.Errorf("directory is not owned by the current user: %s", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("directory has no restrictive DACL: %s", path)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("unsupported directory DACL entry: %s", path)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Mask != 0 && !sid.Equals(user.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return fmt.Errorf("directory grants another principal access: %s", path)
		}
	}
	return nil
}

// A persistent file keyed by the kernel directory identity serializes aliases
// and independent owners without writing a lock file into a user's checkout.
func OpenDirectoryLock(path string) (*os.File, error) {
	h, err := openHandle(path, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING, 0, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return nil, errors.New("workspace lock target is not a directory")
	}
	// Use the account's kernel-resolved folder so differing APPDATA values
	// cannot create independent locks for the same checkout.
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(base, "Vantry", "workspace-locks")
	if err := EnsurePrivateDir(lockDir); err != nil {
		return nil, err
	}
	return OpenNoFollow(filepath.Join(lockDir, fmt.Sprintf("%08x-%08x%08x.lock", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)), os.O_CREATE|os.O_RDWR, 0o600)
}

func TryLock(file *os.File) error {
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		return err
	}
	active, err := hostprocess.JobActive(file.Name())
	if err != nil || active {
		_ = Unlock(file)
		if err != nil {
			return err
		}
		return windows.ERROR_LOCK_VIOLATION
	}
	return nil
}

func Unlock(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func LockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

// Windows executability is checked by CreateProcess and the file's ACL, rather
// than Unix mode bits. The caller still verifies the entry is a regular file.
func Executable(info os.FileInfo) bool { return info.Mode().IsRegular() }
