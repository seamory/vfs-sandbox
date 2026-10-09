//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type layerChange struct {
	Path string
	Kind string
}

type upperFile struct {
	Path       string
	Source     string
	MountDepth int
	Info       fs.FileInfo
}

func runDiff(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: vfs-sandbox diff LAYER_DIR")
		return 2
	}
	layerDir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: resolve layer directory: %v\n", err)
		return 2
	}
	manifestFile, err := os.Open(filepath.Join(layerDir, "manifest.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: open manifest: %v\n", err)
		return 2
	}
	var manifest layerManifest
	decodeErr := json.NewDecoder(manifestFile).Decode(&manifest)
	closeErr := manifestFile.Close()
	if decodeErr != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: decode manifest: %v\n", decodeErr)
		return 2
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: close manifest: %v\n", closeErr)
		return 2
	}
	if manifest.Version != 1 {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: unsupported manifest version %d\n", manifest.Version)
		return 2
	}

	var changes []layerChange
	for _, mount := range manifest.Mounts {
		if mount.UpperDir == "" {
			continue
		}
		upperRel := filepath.Clean(filepath.FromSlash(mount.UpperDir))
		if !filepath.IsLocal(upperRel) || upperRel == "." {
			fmt.Fprintf(os.Stderr, "vfs-sandbox diff: unsafe upper path in manifest: %q\n", mount.UpperDir)
			return 2
		}
		root, err := resolveUpperDirectory(layerDir, upperRel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox diff: open upper layer %s: %v\n", upperRel, err)
			return 2
		}
		mountChanges, err := scanUpper(root, mount.MountPoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox diff: scan %s: %v\n", mount.MountPoint, err)
			return 2
		}
		changes = append(changes, mountChanges...)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Kind < changes[j].Kind
	})
	if len(changes) == 0 {
		fmt.Println("No filesystem changes were recorded.")
		return 0
	}
	for _, change := range changes {
		fmt.Printf("%-22s %s\n", change.Kind, change.Path)
	}
	return 0
}

func runExport(args []string) int {
	return runExportWithOutput(args, os.Stdout)
}

func runExportWithOutput(args []string, output io.Writer) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: vfs-sandbox export LAYER_DIR [DEST_DIR]")
		return 2
	}
	layerDir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox export: resolve layer directory: %v\n", err)
		return 2
	}
	manifestFile, err := os.Open(filepath.Join(layerDir, "manifest.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox export: open manifest: %v\n", err)
		return 2
	}
	var manifest layerManifest
	decodeErr := json.NewDecoder(manifestFile).Decode(&manifest)
	closeErr := manifestFile.Close()
	if decodeErr != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox export: decode manifest: %v\n", decodeErr)
		return 2
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox export: close manifest: %v\n", closeErr)
		return 2
	}
	if manifest.Version != 1 {
		fmt.Fprintf(os.Stderr, "vfs-sandbox export: unsupported manifest version %d\n", manifest.Version)
		return 2
	}

	files := make(map[string]upperFile)
	for _, mount := range manifest.Mounts {
		if mount.UpperDir == "" {
			continue
		}
		upperRel := filepath.Clean(filepath.FromSlash(mount.UpperDir))
		if !filepath.IsLocal(upperRel) || upperRel == "." {
			fmt.Fprintf(os.Stderr, "vfs-sandbox export: unsafe upper path in manifest: %q\n", mount.UpperDir)
			return 2
		}
		root, err := resolveUpperDirectory(layerDir, upperRel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox export: open upper layer %s: %v\n", upperRel, err)
			return 2
		}
		mountFiles, err := scanUpperFiles(root, mount.MountPoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox export: scan %s: %v\n", mount.MountPoint, err)
			return 2
		}
		for _, file := range mountFiles {
			previous, exists := files[file.Path]
			if !exists || file.MountDepth > previous.MountDepth {
				files[file.Path] = file
			}
		}
	}

	requestedDestination := ""
	if len(args) == 2 {
		requestedDestination = args[1]
	}
	destination, err := createExportDirectory(layerDir, requestedDestination)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox export: %v\n", err)
		return 2
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var totalBytes int64
	for _, path := range paths {
		file := files[path]
		relative := filepath.FromSlash(strings.TrimPrefix(file.Path, "/"))
		if !filepath.IsLocal(relative) || relative == "." {
			fmt.Fprintf(os.Stderr, "vfs-sandbox export: unsafe changed path: %q\n", file.Path)
			return 2
		}
		written, err := copyExportFile(file.Source, filepath.Join(destination, relative), file.Info)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox export: copy %s: %v\n", file.Path, err)
			return 2
		}
		totalBytes += written
	}
	complete = true
	fmt.Fprintf(output, "Exported %d file(s), %d bytes, to %s\n", len(files), totalBytes, destination)
	return 0
}

func scanUpperFiles(upper, mountpoint string) ([]upperFile, error) {
	var files []upperFile
	err := filepath.WalkDir(upper, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == upper {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(upper, path)
		if err != nil {
			return err
		}
		virtualPath := overlayVirtualPath(mountpoint, filepath.ToSlash(relative))
		files = append(files, upperFile{
			Path:       virtualPath,
			Source:     path,
			MountDepth: pathDepth(cleanMountPoint(mountpoint)),
			Info:       info,
		})
		return nil
	})
	return files, err
}

func createExportDirectory(layerDir, requested string) (string, error) {
	layerRoot, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return "", fmt.Errorf("resolve layer directory: %w", err)
	}
	if requested == "" {
		parent, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve current directory for artifacts: %w", err)
		}
		parent, err = filepath.EvalSymlinks(parent)
		if err != nil {
			return "", fmt.Errorf("resolve current directory for artifacts: %w", err)
		}
		pattern := "vfs-sandbox-artifacts-" + time.Now().Format("20060102-150405-")
		destination, err := os.MkdirTemp(parent, pattern)
		if err != nil {
			return "", fmt.Errorf("create artifact directory: %w", err)
		}
		if isPathWithin(destination, layerRoot) {
			_ = os.RemoveAll(destination)
			return "", errors.New("artifact directory must be outside the retained layer")
		}
		if err := os.Chmod(destination, 0700); err != nil {
			_ = os.RemoveAll(destination)
			return "", fmt.Errorf("protect artifact directory: %w", err)
		}
		return destination, nil
	}
	destination, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve destination directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return "", fmt.Errorf("resolve destination parent: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if isPathWithin(destination, layerRoot) {
		return "", errors.New("destination directory must be outside the retained layer")
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("destination directory %q already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect destination directory: %w", err)
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return "", fmt.Errorf("create destination directory: %w", err)
	}
	return destination, nil
}

func validateExportDirectory(layerDir, requested string) error {
	layerRoot, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return fmt.Errorf("resolve layer directory: %w", err)
	}
	if requested == "" {
		parent, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve current directory for artifacts: %w", err)
		}
		parent, err = filepath.EvalSymlinks(parent)
		if err != nil {
			return fmt.Errorf("resolve current directory for artifacts: %w", err)
		}
		if isPathWithin(parent, layerRoot) {
			return errors.New("artifact directory must be outside the retained layer")
		}
		return nil
	}
	destination, err := filepath.Abs(requested)
	if err != nil {
		return fmt.Errorf("resolve destination directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("resolve destination parent: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if isPathWithin(destination, layerRoot) {
		return errors.New("destination directory must be outside the retained layer")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("destination directory %q already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination directory: %w", err)
	}
	return nil
}

func copyExportFile(sourcePath, destinationPath string, expected fs.FileInfo) (int64, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return 0, err
	}
	openedInfo, err := source.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(expected, openedInfo) {
		_ = source.Close()
		if err != nil {
			return 0, err
		}
		return 0, errors.New("source file changed during export")
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0700); err != nil {
		_ = source.Close()
		return 0, err
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, expected.Mode().Perm())
	if err != nil {
		_ = source.Close()
		return 0, err
	}
	written, copyErr := io.Copy(destination, source)
	sourceCloseErr := source.Close()
	destinationCloseErr := destination.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if sourceCloseErr != nil {
		return 0, sourceCloseErr
	}
	if destinationCloseErr != nil {
		return 0, destinationCloseErr
	}
	if err := os.Chtimes(destinationPath, expected.ModTime(), expected.ModTime()); err != nil {
		return 0, err
	}
	return written, nil
}

func resolveUpperDirectory(layerDir, relative string) (string, error) {
	root, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return "", err
	}
	components := strings.Split(relative, string(filepath.Separator))
	current := root
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("unsafe path component %q", component)
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("upper layer path contains a symlink at %s", current)
		}
		if index < len(components)-1 && !info.IsDir() {
			return "", fmt.Errorf("upper layer path component %s is not a directory", current)
		}
		if index == len(components)-1 && !info.IsDir() {
			return "", fmt.Errorf("upper layer %s is not a directory", current)
		}
	}
	return current, nil
}

func scanUpper(upper, mountpoint string) ([]layerChange, error) {
	var changes []layerChange
	err := filepath.WalkDir(upper, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == upper {
			return nil
		}
		relative, err := filepath.Rel(upper, path)
		if err != nil {
			return err
		}
		virtualPath := overlayVirtualPath(mountpoint, filepath.ToSlash(relative))
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if isOverlayWhiteout(path, info) {
			changes = append(changes, layerChange{Path: virtualPath, Kind: "deleted"})
			return nil
		}
		if info.IsDir() && hasOpaqueXattr(path) {
			changes = append(changes, layerChange{Path: virtualPath, Kind: "opaque directory"})
		}
		kind := "changed"
		if info.IsDir() {
			kind = "directory"
		} else if info.Mode()&os.ModeSymlink != 0 {
			kind = "symlink"
		} else if !info.Mode().IsRegular() {
			kind = "special"
		}
		changes = append(changes, layerChange{Path: virtualPath, Kind: kind})
		return nil
	})
	return changes, err
}

func overlayVirtualPath(mountpoint, relative string) string {
	if relative == "." || relative == "" {
		return cleanMountPoint(mountpoint)
	}
	return filepath.ToSlash(filepath.Join(cleanMountPoint(mountpoint), relative))
}

func isOverlayWhiteout(path string, info os.FileInfo) bool {
	if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice == 0 {
		return hasXattr(path, "trusted.overlay.whiteout") || hasXattr(path, "user.overlay.whiteout")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Rdev == 0
}

func hasOpaqueXattr(path string) bool {
	for _, name := range []string{"trusted.overlay.opaque", "user.overlay.opaque"} {
		value, err := getXattr(path, name)
		if err == nil && (string(value) == "y" || string(value) == "x") {
			return true
		}
	}
	return false
}

func hasXattr(path, name string) bool {
	value, err := getXattr(path, name)
	return err == nil && (len(value) == 0 || string(value) == "y" || string(value) == "x")
}

func getXattr(path, name string) ([]byte, error) {
	size, err := syscall.Getxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	value := make([]byte, size)
	if size == 0 {
		return value, nil
	}
	n, err := syscall.Getxattr(path, name, value)
	return value[:n], err
}
