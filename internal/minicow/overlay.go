// Package minicow manages the file view used by the Windows minifilter broker.
// A session owns one Overlay; its requests must be serialized by the caller.
package minicow

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Overlay uses a private tree for modified files. Known paths that disappear
// from that tree are whiteouts, including when removed through an open handle.
// It must never be reused after the owning process tree has finished.
type Overlay struct {
	Root         string
	Volumes      map[string]string
	known        map[string]bool
	listed       map[string]bool
	placeholders map[string]bool
	baseline     map[string]string
	opaque       map[string]bool
	names        map[string]string
}

func New(root string, volumes map[string]string) (*Overlay, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Overlay{Root: root, Volumes: volumes, known: map[string]bool{}, listed: map[string]bool{}, placeholders: map[string]bool{}, baseline: map[string]string{}, opaque: map[string]bool{}, names: map[string]string{}}, nil
}

func key(path string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))))
}
func (o *Overlay) paths(path string) (string, string, error) {
	path = filepath.Clean(filepath.FromSlash(path))
	if !filepath.IsLocal(path) || path == "." {
		return "", "", fs.ErrPermission
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	root, ok := o.Volumes[strings.ToUpper(parts[0])]
	if !ok {
		return "", "", fs.ErrPermission
	}
	for _, p := range parts {
		if strings.ContainsAny(p, ":*?") || strings.TrimRight(p, " .") != p {
			return "", "", fs.ErrPermission
		}
	}
	parts[0] = strings.ToUpper(parts[0])
	logical := strings.Join(parts, "/")
	if _, exists := o.names[key(logical)]; !exists {
		o.names[key(logical)] = logical
	}
	return filepath.Join(append([]string{root}, parts[1:]...)...), filepath.Join(append([]string{o.Root}, parts...)...), nil
}

// checked rejects reparse-point ancestors rather than following them outside
// the selected lower volume or private layer.
func checked(path string) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return nil, fs.ErrPermission
	}
	if err = validatePlatformPath(path, info); err != nil {
		return nil, err
	}
	return info, nil
}

func (o *Overlay) hiddenLower(path string) bool {
	for parent := filepath.Dir(filepath.FromSlash(path)); parent != "."; parent = filepath.Dir(parent) {
		if o.opaque[key(parent)] {
			return true
		}
	}
	return false
}

func (o *Overlay) parent(path string) error {
	p := filepath.Dir(filepath.FromSlash(path))
	if p == "." {
		return nil
	}
	_, err := o.Resolve(filepath.ToSlash(p), false, true)
	return err
}

// Resolve returns a physical path. Writable opens copy up existing files;
// read-only opens keep reading the host until that file has been copied up.
// Directories always use private handles so namespace mutations stay private.
func (o *Overlay) Resolve(path string, writable, directory bool) (string, error) {
	lower, upper, err := o.paths(path)
	if err != nil {
		return "", err
	}
	if err = o.parent(path); err != nil {
		return "", err
	}
	k := key(path)
	if o.known[k] {
		info, e := checked(upper)
		if e != nil && !(writable && errors.Is(e, fs.ErrNotExist)) {
			return "", e
		}
		if errors.Is(e, fs.ErrNotExist) && writable {
			if directory {
				o.opaque[k] = true
			}
			delete(o.placeholders, k)
		}
		if e == nil && info.IsDir() && writable {
			if e = o.List(path); e != nil {
				return "", e
			}
		}
		if e == nil && o.placeholders[k] {
			if !writable {
				return lower, nil
			}
			original, e := checked(lower)
			if e != nil {
				return "", e
			}
			temp, e := os.CreateTemp(filepath.Dir(upper), ".copy-up-*")
			if e != nil {
				return "", e
			}
			temporary := temp.Name()
			temp.Close()
			os.Remove(temporary)
			if e = copyFile(lower, temporary, original); e != nil {
				return "", e
			}
			if e = os.Rename(temporary, upper); e != nil {
				os.Remove(temporary)
				return "", e
			}
			delete(o.placeholders, k)
			o.baseline[k], e = fingerprint(upper)
			if e != nil {
				return "", e
			}
		}
		return upper, nil
	}
	info, err := checked(lower)
	if o.hiddenLower(path) {
		info = nil
		err = fs.ErrNotExist
	}
	if err != nil {
		if writable && errors.Is(err, fs.ErrNotExist) {
			o.known[k] = true
			if directory {
				o.opaque[k] = true
			}
			return upper, nil
		}
		return "", err
	}
	if info.IsDir() {
		if err = os.MkdirAll(upper, 0700); err != nil {
			return "", err
		}
		o.known[k] = true
		o.baseline[k] = "directory"
		if writable {
			if err = o.List(path); err != nil {
				return "", err
			}
		}
		return upper, nil
	}
	if directory {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	if !writable {
		return lower, nil
	}
	if err = copyFile(lower, upper, info); err != nil {
		return "", err
	}
	o.known[k] = true
	o.baseline[k], err = fingerprint(upper)
	if err != nil {
		return "", err
	}
	return upper, nil
}

// List materializes the direct children before the kernel enumerates a private
// directory using sparse metadata placeholders. File opens are still resolved
// against the lower file until first write; placeholders are never served as
// file contents. Subdirectories are populated lazily.
func (o *Overlay) List(path string) error {
	lower, upper, err := o.paths(path)
	if err != nil {
		return err
	}
	if _, err = checked(upper); err != nil {
		return err
	}
	if o.listed[key(path)] || o.opaque[key(path)] || o.hiddenLower(path) {
		return nil
	}
	entries, err := os.ReadDir(lower)
	if errors.Is(err, fs.ErrNotExist) {
		o.listed[key(path)] = true
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.ToSlash(filepath.Join(path, entry.Name()))
		if o.known[key(child)] {
			continue
		}
		src := filepath.Join(lower, entry.Name())
		dst := filepath.Join(upper, entry.Name())
		info, e := checked(src)
		// Reparse entries remain inaccessible, including through directory handles.
		if errors.Is(e, fs.ErrPermission) {
			continue
		}
		if e != nil {
			return e
		}
		if info.IsDir() {
			e = os.Mkdir(dst, 0700)
			if e == nil {
				o.baseline[key(child)] = "directory"
			}
		} else {
			e = placeholder(dst, info)
			if e == nil {
				o.placeholders[key(child)] = true
				o.baseline[key(child)] = "placeholder"
			}
		}
		if e != nil {
			return e
		}
		o.known[key(child)] = true
		o.names[key(child)] = child
	}
	o.listed[key(path)] = true
	return nil
}

func (o *Overlay) materialize(path string) error {
	physical, err := o.Resolve(path, true, false)
	if err != nil {
		return err
	}
	info, err := checked(physical)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if err = o.List(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(physical)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = o.materialize(filepath.ToSlash(filepath.Join(path, entry.Name()))); err != nil {
			return err
		}
	}
	return nil
}

// Rename executes outside the filtered process, then moves the session's
// whiteouts with the subtree. The original source is retained as a whiteout.
func (o *Overlay) Rename(source, target string, replace bool) error {
	if key(source) == key(target) {
		return fs.ErrInvalid
	}
	if strings.Split(key(source), "/")[0] != strings.Split(key(target), "/")[0] {
		return fs.ErrPermission
	}
	if strings.HasPrefix(key(target), key(source)+"/") {
		return fs.ErrInvalid
	}
	if err := o.materialize(source); err != nil {
		return err
	}
	dest, err := o.Resolve(target, true, false)
	if err != nil {
		return err
	}
	if _, err = checked(dest); err == nil && !replace {
		return fs.ErrExist
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, src, err := o.paths(source)
	if err != nil {
		return err
	}
	if err = os.Rename(src, dest); err != nil {
		return err
	}
	old, newKey := key(source), key(target)
	// Preserve filename spelling for physical lookups and reports. The folded
	// map key is an identity only, not necessarily a valid filesystem spelling.
	movedNames := map[string]string{}
	sourceDepth := len(strings.Split(filepath.ToSlash(source), "/"))
	for k, name := range o.names {
		if k == old || strings.HasPrefix(k, old+"/") {
			suffix := strings.Split(filepath.ToSlash(name), "/")[sourceDepth:]
			movedNames[newKey+strings.TrimPrefix(k, old)] = strings.Join(append([]string{filepath.ToSlash(target)}, suffix...), "/")
		}
	}
	for k, name := range movedNames {
		o.names[k] = name
	}
	move := func(m map[string]bool) {
		updates := map[string]bool{}
		for k, v := range m {
			if k == old || strings.HasPrefix(k, old+"/") {
				updates[newKey+strings.TrimPrefix(k, old)] = v
				delete(m, k)
			}
		}
		for k := range m {
			if k == newKey || strings.HasPrefix(k, newKey+"/") {
				delete(m, k)
			}
		}
		for k, v := range updates {
			m[k] = v
		}
	}
	move(o.known)
	move(o.listed)
	move(o.placeholders)
	move(o.opaque)
	o.opaque[newKey] = true
	o.known[old] = true
	return nil
}

// ExportView materializes a requested subtree for the existing diff/artifact API.
func (o *Overlay) ExportView(path, destination string) error {
	if err := o.materialize(path); err != nil {
		return err
	}
	_, source, err := o.paths(path)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(source, p)
		if e != nil {
			return e
		}
		dst := filepath.Join(destination, rel)
		info, e := checked(p)
		if e != nil {
			return e
		}
		if info.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		return copyFile(p, dst, info)
	})
}

func copyFile(source, destination string, info fs.FileInfo) (err error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		out.Close()
		if !complete {
			os.Remove(destination)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chmod(destination, info.Mode().Perm()); err != nil {
		return err
	}
	if err = os.Chtimes(destination, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	complete = true
	return nil
}
