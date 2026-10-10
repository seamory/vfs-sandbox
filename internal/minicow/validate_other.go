//go:build !windows

package minicow

import "io/fs"

func validatePlatformPath(_ string, _ fs.FileInfo) error { return nil }
