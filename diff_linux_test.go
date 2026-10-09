//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestScanUpperReportsChangesWhiteoutsAndOpaqueDirectories(t *testing.T) {
	upper, err := os.MkdirTemp("", "vfs-diff-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(upper) })
	if err := os.Mkdir(filepath.Join(upper, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"sub/changed": "new value",
	} {
		if err := os.WriteFile(filepath.Join(upper, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	whiteoutCreated := false
	if err := syscall.Mknod(filepath.Join(upper, "sub", "removed"), syscall.S_IFCHR|0600, 0); err == nil {
		whiteoutCreated = true
	} else if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatal(err)
	}
	opaqueCreated := false
	if err := syscall.Setxattr(filepath.Join(upper, "sub"), "user.overlay.opaque", []byte("y"), 0); err == nil {
		opaqueCreated = true
	} else if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatal(err)
	}

	changes, err := scanUpper(upper, "/mnt/data")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"directory /mnt/data/sub":       "directory",
		"changed /mnt/data/sub/changed": "changed",
	}
	if whiteoutCreated {
		want["deleted /mnt/data/sub/removed"] = "deleted"
	}
	if opaqueCreated {
		want["opaque directory /mnt/data/sub"] = "opaque directory"
	}
	if len(changes) != len(want) {
		t.Fatalf("got %d changes, want %d: %#v", len(changes), len(want), changes)
	}
	for _, change := range changes {
		key := change.Kind + " " + change.Path
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected change %q", key)
		}
	}
}

func TestResolveUpperDirectoryRejectsSymlinks(t *testing.T) {
	layer := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(layer, "root")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveUpperDirectory(layer, filepath.Join("root", "upper")); err == nil {
		t.Fatal("expected symlink upper path to be rejected")
	}
}
