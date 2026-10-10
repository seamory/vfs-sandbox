package minicow

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) (*Overlay, string) {
	t.Helper()
	root := t.TempDir()
	lower := filepath.Join(root, "host")
	if e := os.Mkdir(lower, 0700); e != nil {
		t.Fatal(e)
	}
	o, e := New(filepath.Join(root, "layer"), map[string]string{"C": lower})
	if e != nil {
		t.Fatal(e)
	}
	return o, lower
}
func write(t *testing.T, path, value string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
}
func read(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func TestReadThroughAndCopyUp(t *testing.T) {
	o, host := fixture(t)
	write(t, filepath.Join(host, "a"), "original")
	p, e := o.Resolve("C/a", false, false)
	if e != nil || p != filepath.Join(host, "a") {
		t.Fatalf("read: %s %v", p, e)
	}
	p, e = o.Resolve("C/a", true, false)
	if e != nil {
		t.Fatal(e)
	}
	write(t, p, "private")
	if read(t, filepath.Join(host, "a")) != "original" {
		t.Fatal("host modified")
	}
	p, e = o.Resolve("C/a", false, false)
	if e != nil || read(t, p) != "private" {
		t.Fatal("copy-up not visible", e)
	}
	c, e := o.Changes()
	if e != nil || len(c) != 1 || c[0].Kind != "changed" {
		t.Fatalf("changes: %+v %v", c, e)
	}
}
func TestEnumerationPlaceholdersNeverBecomeContents(t *testing.T) {
	o, host := fixture(t)
	write(t, filepath.Join(host, "a"), "original")
	if _, e := o.Resolve("C", false, true); e != nil {
		t.Fatal(e)
	}
	if e := o.List("C"); e != nil {
		t.Fatal(e)
	}
	p, e := o.Resolve("C/a", false, false)
	if e != nil || read(t, p) != "original" {
		t.Fatal("placeholder leaked", e)
	}
	c, e := o.Changes()
	if e != nil || len(c) != 0 {
		t.Fatalf("enumeration changed files: %+v %v", c, e)
	}
	p, e = o.Resolve("C/a", true, false)
	if e != nil || read(t, p) != "original" {
		t.Fatal("copy-up failed", e)
	}
	write(t, p, "private")
	if read(t, filepath.Join(host, "a")) != "original" {
		t.Fatal("host modified")
	}
}
func TestDeletionAndRecreation(t *testing.T) {
	o, host := fixture(t)
	write(t, filepath.Join(host, "a"), "old")
	p, e := o.Resolve("C/a", true, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if _, e = o.Resolve("C/a", false, false); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("deleted host file reappeared", e)
	}
	c, e := o.Changes()
	if e != nil || len(c) != 1 || c[0].Kind != "deleted" {
		t.Fatalf("changes: %+v %v", c, e)
	}
	p, e = o.Resolve("C/a", true, false)
	if e != nil {
		t.Fatal(e)
	}
	write(t, p, "new")
	p, e = o.Resolve("C/a", false, false)
	if e != nil || read(t, p) != "new" {
		t.Fatal("recreation failed", e)
	}
	if read(t, filepath.Join(host, "a")) != "old" {
		t.Fatal("host modified")
	}
}
func TestDirectoryRenamePreservesChildrenAndWhiteouts(t *testing.T) {
	o, host := fixture(t)
	os.Mkdir(filepath.Join(host, "old"), 0700)
	write(t, filepath.Join(host, "old", "keep"), "keep")
	write(t, filepath.Join(host, "old", "gone"), "gone")
	p, e := o.Resolve("C/old/gone", true, false)
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(p)
	if e = o.Rename("C/old", "C/new", false); e != nil {
		t.Fatal(e)
	}
	p, e = o.Resolve("C/new/keep", false, false)
	if e != nil || read(t, p) != "keep" {
		t.Fatal("rename lost child", e)
	}
	if _, e = o.Resolve("C/new/gone", false, false); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("rename resurrected deletion", e)
	}
	if _, e = o.Resolve("C/old/keep", false, false); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("old path resurrected", e)
	}
	if read(t, filepath.Join(host, "old", "gone")) != "gone" {
		t.Fatal("host modified")
	}
}
func TestExportHydratesFiles(t *testing.T) {
	o, host := fixture(t)
	write(t, filepath.Join(host, "a"), "bytes")
	out := filepath.Join(t.TempDir(), "export")
	if e := o.ExportView("C", out); e != nil {
		t.Fatal(e)
	}
	if read(t, filepath.Join(out, "a")) != "bytes" {
		t.Fatal("exported placeholder")
	}
	c, e := o.Changes()
	if e != nil || len(c) != 0 {
		t.Fatalf("export recorded changes: %+v %v", c, e)
	}
}
func TestPathBoundariesAndSymlink(t *testing.T) {
	o, host := fixture(t)
	for _, p := range []string{"../escape", "D/file", "C/a:stream", "C/a.", "C/../escape"} {
		if _, e := o.Resolve(p, true, false); e == nil {
			t.Fatal("accepted", p)
		}
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "a"), "host")
	if e := os.Symlink(outside, filepath.Join(host, "link")); e != nil {
		t.Skip(e)
	}
	if _, e := o.Resolve("C/link/a", true, false); e == nil {
		t.Fatal("followed symlink")
	}
}
func TestCrossVolumeRenameRejected(t *testing.T) {
	o, _ := fixture(t)
	if e := o.Rename("C/a", "D/a", true); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
}

func TestRecreatedDirectoryIsOpaque(t *testing.T) {
	o, host := fixture(t)
	if e := os.Mkdir(filepath.Join(host, "dir"), 0700); e != nil {
		t.Fatal(e)
	}
	p, e := o.Resolve("C/dir", true, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(p); e != nil {
		t.Fatal(e)
	}
	p, e = o.Resolve("C/dir", true, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(p, 0700); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(host, "dir", "late"), "host")
	if _, e = o.Resolve("C/dir/late", false, false); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("lower entry leaked into recreated directory", e)
	}
	if e = o.List("C/dir"); e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(p)
	if e != nil || len(entries) != 0 {
		t.Fatal("recreated directory is not empty", entries, e)
	}
}
func TestFailedCreateDoesNotRecordChange(t *testing.T) {
	o, _ := fixture(t)
	if _, e := o.Resolve("C/not-created", true, false); e != nil {
		t.Fatal(e)
	}
	c, e := o.Changes()
	if e != nil || len(c) != 0 {
		t.Fatal(c, e)
	}
}
func TestReplaceRenameReportsBothPaths(t *testing.T) {
	o, host := fixture(t)
	write(t, filepath.Join(host, "from"), "new")
	write(t, filepath.Join(host, "to"), "old")
	if e := o.Rename("C/from", "C/to", true); e != nil {
		t.Fatal(e)
	}
	p, e := o.Resolve("C/to", false, false)
	if e != nil || read(t, p) != "new" {
		t.Fatal(e)
	}
	c, e := o.Changes()
	if e != nil || len(c) != 2 {
		t.Fatal(c, e)
	}
	if c[0].Path != "C/from" || c[0].Kind != "deleted" || c[1].Kind != "changed" {
		t.Fatal(c)
	}
	if read(t, filepath.Join(host, "to")) != "old" || read(t, filepath.Join(host, "from")) != "new" {
		t.Fatal("host changed")
	}
}
