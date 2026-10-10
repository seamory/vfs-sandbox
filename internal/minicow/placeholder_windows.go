package minicow

import (
	"io/fs"
	"os"
	"syscall"
)

func placeholder(path string, info fs.FileInfo) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	var returned uint32
	// Sparse placeholders retain the apparent length without allocating content.
	if err = syscall.DeviceIoControl(syscall.Handle(file.Fd()), 0x900c4, nil, 0, nil, 0, &returned, nil); err != nil {
		return err
	}
	if err = file.Truncate(info.Size()); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	complete = true
	return nil
}
