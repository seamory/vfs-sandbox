package minicow

import (
	"fmt"
	"io/fs"
	"syscall"
	"unsafe"
)

var fileInformation = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileInformationByHandleEx")

func validatePlatformPath(path string, info fs.FileInfo) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := syscall.GetFileAttributes(name)
	if err != nil {
		return err
	}
	if attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fs.ErrPermission
	}
	if !info.IsDir() {
		return nil
	}
	handle, err := syscall.CreateFile(name, 0x80 /* FILE_READ_ATTRIBUTES */, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)
	var flags uint32
	ok, _, err := fileInformation.Call(uintptr(handle), 23, uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags)) // FileCaseSensitiveInfo
	if ok == 0 {
		return fmt.Errorf("query NTFS case-sensitivity: %w", err)
	}
	if flags&1 != 0 {
		return fmt.Errorf("case-sensitive directory %q: %w", path, fs.ErrPermission)
	}
	return nil
}
