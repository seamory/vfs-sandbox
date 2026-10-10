//go:build !windows

package minicow

import (
	"io/fs"
	"os"
)

// Sparse files on the host make overlay semantics testable without a driver.
func placeholder(path string, info fs.FileInfo) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = file.Truncate(info.Size()); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Chtimes(path, info.ModTime(), info.ModTime())
}
