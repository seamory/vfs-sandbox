//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type layerManifest struct {
	Version   int             `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	Mounts    []manifestMount `json:"mounts"`
}

type manifestMount struct {
	ID         int         `json:"mount_id,omitempty"`
	MountPoint string      `json:"mount_point"`
	Filesystem string      `json:"filesystem,omitempty"`
	Source     string      `json:"source,omitempty"`
	Policy     mountPolicy `json:"policy"`
	UpperDir   string      `json:"upper_dir,omitempty"`
}

type overlayLayerDirs struct {
	Upper string
	Work  string
}

func setupAndExec(layerDir, command string, commandArgs []string) error {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get starting directory: %w", err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount propagation private: %w", err)
	}

	mounts, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	plan := planMounts(mounts)
	root := findRootMount(plan)
	if root == nil {
		return errors.New("mount table does not contain the root filesystem")
	}

	lowerDir := filepath.Join(layerDir, "lower")
	mergedDir := filepath.Join(layerDir, "merged")
	if err := os.MkdirAll(lowerDir, 0700); err != nil {
		return fmt.Errorf("create lower mount point: %w", err)
	}
	if err := os.MkdirAll(mergedDir, 0700); err != nil {
		return fmt.Errorf("create merged mount point: %w", err)
	}
	if err := syscall.Mount("/", lowerDir, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind the host root as the lower tree: %w", err)
	}

	manifest := layerManifest{Version: 1, CreatedAt: time.Now().UTC()}
	rootUpper := filepath.Join(layerDir, "root", "upper")
	rootWork := filepath.Join(layerDir, "root", "work")
	if err := makeOverlayDirectories(rootUpper, rootWork); err != nil {
		return err
	}
	overlayDirs := map[string]overlayLayerDirs{"/": {Upper: rootUpper, Work: rootWork}}
	for _, entry := range plan {
		mountpoint := entry.Mount.MountPoint
		if mountpoint == "/" || entry.Policy != policyOverlay || isExplicitlyReplacedPath(mountpoint) || isSuppressedByAncestor(mountpoint, plan) {
			continue
		}
		sourceInfo, err := os.Stat(rootedPath(lowerDir, mountpoint))
		if err != nil {
			return fmt.Errorf("inspect lower mount %q: %w", mountpoint, err)
		}
		if !sourceInfo.IsDir() {
			continue
		}
		upper := filepath.Join(layerDir, "mounts", fmt.Sprintf("%06d", entry.Mount.ID), "upper")
		work := filepath.Join(layerDir, "mounts", fmt.Sprintf("%06d", entry.Mount.ID), "work")
		if err := makeOverlayDirectories(upper, work); err != nil {
			return err
		}
		overlayDirs[mountpoint] = overlayLayerDirs{Upper: upper, Work: work}
	}
	if err := materializeFileMounts(plan, lowerDir, overlayDirs); err != nil {
		return err
	}
	if err := mountOverlay(lowerDir, rootUpper, rootWork, mergedDir); err != nil {
		return fmt.Errorf("mount root OverlayFS: %w", err)
	}
	manifest.Mounts = append(manifest.Mounts, manifestMount{
		ID: root.Mount.ID, MountPoint: "/", Filesystem: root.Mount.Filesystem,
		Source: root.Mount.Source, Policy: policyOverlay, UpperDir: filepath.ToSlash(filepath.Join("root", "upper")),
	})

	if err := setupCoreMounts(mergedDir); err != nil {
		return err
	}

	for _, entry := range plan {
		mountpoint := entry.Mount.MountPoint
		if mountpoint == "/" {
			continue
		}
		if shouldSkipPlannedMount(mergedDir, mountpoint) || isSuppressedByAncestor(mountpoint, plan) {
			manifest.Mounts = append(manifest.Mounts, manifestMount{
				ID: entry.Mount.ID, MountPoint: mountpoint, Filesystem: entry.Mount.Filesystem,
				Source: entry.Mount.Source, Policy: effectiveSkippedPolicy(mountpoint, entry.Policy, plan),
			})
			continue
		}
		record := manifestMount{
			ID: entry.Mount.ID, MountPoint: mountpoint, Filesystem: entry.Mount.Filesystem,
			Source: entry.Mount.Source, Policy: entry.Policy,
		}
		sourcePath := rootedPath(lowerDir, mountpoint)
		targetPath := rootedPath(mergedDir, mountpoint)
		sourceInfo, err := os.Stat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect lower mount %q: %w", mountpoint, err)
		}
		if !sourceInfo.IsDir() {
			if sourceInfo.Mode()&(os.ModeSocket|os.ModeDevice|os.ModeNamedPipe) != 0 || entry.Policy == policyKernelMask {
				record.Policy = policyMaskFile
				manifest.Mounts = append(manifest.Mounts, record)
				continue
			}
			if _, err := ensureMountFileTarget(mergedDir, mountpoint, sourceInfo.Mode().Perm()); err != nil {
				return fmt.Errorf("prepare file mount target %q: %w", mountpoint, err)
			}
			switch entry.Policy {
			case policyOverlay:
				if !sourceInfo.Mode().IsRegular() {
					return fmt.Errorf("file mount %q has unsupported source type %s", mountpoint, sourceInfo.Mode().Type())
				}
				record.Policy = policyOverlayFile
			case policyReadOnly:
				if err := mountFileReadOnly(sourcePath, targetPath); err != nil {
					return fmt.Errorf("mount shared file %q read-only: %w", mountpoint, err)
				}
			default:
				return fmt.Errorf("file mount %q has unsupported policy %q", mountpoint, entry.Policy)
			}
			manifest.Mounts = append(manifest.Mounts, record)
			continue
		}
		if _, err := ensureMountTarget(mergedDir, mountpoint); err != nil {
			return fmt.Errorf("prepare mount target %q: %w", mountpoint, err)
		}
		switch entry.Policy {
		case policyOverlay:
			layer, ok := overlayDirs[mountpoint]
			if !ok {
				return fmt.Errorf("missing OverlayFS layer directories for %q", mountpoint)
			}
			if err := mountOverlay(sourcePath, layer.Upper, layer.Work, targetPath); err != nil {
				return fmt.Errorf("overlay mount %q (%s): %w", mountpoint, entry.Mount.Filesystem, err)
			}
			record.UpperDir = filepath.ToSlash(filepath.Join("mounts", fmt.Sprintf("%06d", entry.Mount.ID), "upper"))
		case policyReadOnly:
			if err := mountBindReadOnly(sourcePath, targetPath, mountpoint, plan); err != nil {
				return fmt.Errorf("mount shared filesystem %q read-only: %w", mountpoint, err)
			}
		case policyKernelMask:
			if err := mountTmpfs(targetPath, "mode=0755,size=16m", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC); err != nil {
				return fmt.Errorf("mask kernel interface %q: %w", mountpoint, err)
			}
		case policyProc, policySysfs, policyDevices, policyRuntime, policyRuntimeAlias:
			// These roots and aliases are set up explicitly by the launcher.
			// Their mount-table entries are retained in the manifest for auditing.
		default:
			return fmt.Errorf("unsupported mount policy %q for %q", entry.Policy, mountpoint)
		}
		manifest.Mounts = append(manifest.Mounts, record)
	}

	if err := setupVarTmpMount(mergedDir); err != nil {
		return err
	}
	appendExplicitManifestMounts(&manifest)
	if err := hideLayerDirectory(layerDir, mergedDir); err != nil {
		return err
	}
	if err := writeManifest(layerDir, manifest); err != nil {
		return err
	}
	if err := pivotInto(mergedDir); err != nil {
		return err
	}
	if err := syscall.Chdir(workingDirectory); err != nil {
		return fmt.Errorf("restore working directory %q inside sandbox: %w", workingDirectory, err)
	}
	if err := execTarget(command, commandArgs); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("command %q was not found inside the sandbox root", command)
		}
		return fmt.Errorf("execute command %q inside sandbox: %w", command, err)
	}
	return nil
}

func findRootMount(plan []plannedMount) *plannedMount {
	for i := range plan {
		if plan[i].Mount.MountPoint == "/" {
			return &plan[i]
		}
	}
	return nil
}

func makeOverlayDirectories(upper, work string) error {
	if err := os.MkdirAll(upper, 0700); err != nil {
		return fmt.Errorf("create OverlayFS upper directory %q: %w", upper, err)
	}
	if err := os.MkdirAll(work, 0700); err != nil {
		return fmt.Errorf("create OverlayFS work directory %q: %w", work, err)
	}
	return nil
}

func materializeFileMounts(plan []plannedMount, lowerRoot string, overlayDirs map[string]overlayLayerDirs) error {
	for _, entry := range plan {
		mountpoint := entry.Mount.MountPoint
		if mountpoint == "/" || isExplicitlyReplacedPath(mountpoint) || isSuppressedByAncestor(mountpoint, plan) || pathHasSymlinkComponent(lowerRoot, mountpoint) {
			continue
		}
		source := rootedPath(lowerRoot, mountpoint)
		info, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("inspect lower mount %q: %w", mountpoint, err)
		}
		if info.IsDir() {
			continue
		}
		parentMount, layer := nearestOverlayLayer(mountpoint, overlayDirs)
		relative := relativeMountPath(parentMount, mountpoint)
		if info.Mode()&(os.ModeSocket|os.ModeDevice|os.ModeNamedPipe) != 0 {
			if err := createRootWhiteout(layer.Upper, relative); err != nil {
				return fmt.Errorf("mask shared socket or device at %q: %w", mountpoint, err)
			}
			continue
		}
		switch entry.Policy {
		case policyOverlay:
			if !info.Mode().IsRegular() {
				return fmt.Errorf("file mount %q has unsupported source type %s", mountpoint, info.Mode().Type())
			}
			if err := copyFileToRootUpper(source, layer.Upper, relative, info); err != nil {
				return fmt.Errorf("copy file mount %q into its OverlayFS upper layer: %w", mountpoint, err)
			}
		case policyReadOnly:
			if _, err := ensureMountFileTarget(layer.Upper, relative, info.Mode().Perm()); err != nil {
				return fmt.Errorf("create target for read-only file mount %q: %w", mountpoint, err)
			}
		case policyKernelMask:
			if err := createRootWhiteout(layer.Upper, relative); err != nil {
				return fmt.Errorf("mask kernel file mount %q: %w", mountpoint, err)
			}
		}
	}
	return nil
}

func nearestOverlayLayer(mountpoint string, layers map[string]overlayLayerDirs) (string, overlayLayerDirs) {
	nearest := "/"
	for candidate := range layers {
		if candidate != mountpoint && isPathWithin(mountpoint, candidate) && pathDepth(candidate) > pathDepth(nearest) {
			nearest = candidate
		}
	}
	return nearest, layers[nearest]
}

func relativeMountPath(parent, mountpoint string) string {
	if parent == "/" {
		return cleanMountPoint(mountpoint)
	}
	return "/" + strings.TrimPrefix(cleanMountPoint(mountpoint), parent+"/")
}

func isExplicitlyReplacedPath(mountpoint string) bool {
	for _, root := range []string{"/proc", "/dev", "/run", "/tmp", "/var/tmp", "/dev/shm"} {
		if isPathWithin(mountpoint, root) {
			return true
		}
	}
	return false
}

func isSuppressedByAncestor(mountpoint string, plan []plannedMount) bool {
	var nearest *plannedMount
	for i := range plan {
		candidate := &plan[i]
		parent := candidate.Mount.MountPoint
		if parent == mountpoint || !isPathWithin(mountpoint, parent) {
			continue
		}
		if nearest == nil || pathDepth(parent) > pathDepth(nearest.Mount.MountPoint) {
			nearest = candidate
		}
	}
	if nearest == nil {
		return false
	}
	return nearest.Policy == policyReadOnly || nearest.Policy == policyKernelMask ||
		nearest.Policy == policyProc || nearest.Policy == policyDevices || nearest.Policy == policyRuntime
}

func pathHasSymlinkComponent(root, mountpoint string) bool {
	clean := cleanMountPoint(mountpoint)
	if clean == "/" {
		return false
	}
	current := root
	components := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func copyFileToRootUpper(source, rootUpper, mountpoint string, sourceInfo os.FileInfo) error {
	destination, err := ensureMountFileTarget(rootUpper, mountpoint, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if stat, ok := sourceInfo.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(destination, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
		if err := syscall.Chmod(destination, uint32(stat.Mode)&07777); err != nil {
			return err
		}
	}
	if err := copyXattrs(source, destination); err != nil {
		return err
	}
	return os.Chtimes(destination, sourceInfo.ModTime(), sourceInfo.ModTime())
}

func createRootWhiteout(rootUpper, mountpoint string) error {
	path, err := ensureMountFileTarget(rootUpper, mountpoint, 0600)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syscall.Mknod(path, syscall.S_IFCHR|0600, 0)
}

func copyXattrs(source, destination string) error {
	size, err := syscall.Listxattr(source, nil)
	if err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	if size == 0 {
		return nil
	}
	buffer := make([]byte, size)
	count, err := syscall.Listxattr(source, buffer)
	if err != nil {
		return err
	}
	for _, name := range strings.Split(string(buffer[:count]), "\x00") {
		if name == "" {
			continue
		}
		valueSize, err := syscall.Getxattr(source, name, nil)
		if err != nil {
			return err
		}
		value := make([]byte, valueSize)
		if valueSize > 0 {
			n, err := syscall.Getxattr(source, name, value)
			if err != nil {
				return err
			}
			value = value[:n]
		}
		if err := syscall.Setxattr(destination, name, value, 0); err != nil {
			return err
		}
	}
	return nil
}

func mountOverlay(lower, upper, work, target string) error {
	options := "lowerdir=" + escapeOverlayOption(lower) +
		",upperdir=" + escapeOverlayOption(upper) +
		",workdir=" + escapeOverlayOption(work)
	if err := syscall.Mount("overlay", target, "overlay", 0, options); err != nil {
		return err
	}
	return nil
}

func escapeOverlayOption(path string) string {
	replacer := strings.NewReplacer("\\", "\\\\", ",", "\\,", ":", "\\:")
	return replacer.Replace(path)
}

func setupCoreMounts(root string) error {
	for _, spec := range []struct {
		path  string
		fs    string
		src   string
		flags uintptr
		data  string
	}{
		{path: "/proc", fs: "proc", src: "proc", flags: syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC},
		{path: "/sys", fs: "sysfs", src: "sysfs", flags: syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_RDONLY},
	} {
		target, err := ensureMountTarget(root, spec.path)
		if err != nil {
			return fmt.Errorf("prepare %s mount: %w", spec.path, err)
		}
		if err := syscall.Mount(spec.src, target, spec.fs, spec.flags, spec.data); err != nil {
			return fmt.Errorf("mount fresh %s at %s: %w", spec.fs, spec.path, err)
		}
	}

	dev, err := ensureMountTarget(root, "/dev")
	if err != nil {
		return fmt.Errorf("prepare /dev mount: %w", err)
	}
	if err := mountTmpfs(dev, "mode=0755,size=16m", syscall.MS_NOSUID|syscall.MS_NOEXEC); err != nil {
		return fmt.Errorf("create private /dev: %w", err)
	}
	if err := createDeviceNode(dev, "null", 1, 3); err != nil {
		return err
	}
	if err := createDeviceNode(dev, "zero", 1, 5); err != nil {
		return err
	}
	if err := createDeviceNode(dev, "full", 1, 7); err != nil {
		return err
	}
	if err := createDeviceNode(dev, "random", 1, 8); err != nil {
		return err
	}
	if err := createDeviceNode(dev, "urandom", 1, 9); err != nil {
		return err
	}
	if err := createDeviceNode(dev, "tty", 5, 0); err != nil {
		return err
	}
	for _, link := range []struct{ name, target string }{
		{"stdin", "/proc/self/fd/0"}, {"stdout", "/proc/self/fd/1"}, {"stderr", "/proc/self/fd/2"},
		{"fd", "/proc/self/fd"}, {"ptmx", "pts/ptmx"},
	} {
		if err := os.Symlink(link.target, filepath.Join(dev, link.name)); err != nil {
			return fmt.Errorf("create /dev/%s symlink: %w", link.name, err)
		}
	}

	pts, err := ensureMountTarget(root, "/dev/pts")
	if err != nil {
		return fmt.Errorf("prepare /dev/pts: %w", err)
	}
	if err := syscall.Mount("devpts", pts, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC,
		"newinstance,ptmxmode=0666,mode=0620,gid=5"); err != nil {
		return fmt.Errorf("mount private devpts: %w", err)
	}

	for _, spec := range []struct {
		path string
		data string
	}{
		{path: "/dev/shm", data: "mode=1777,size=64m"},
		{path: "/dev/mqueue", data: "mode=0755,size=16m"},
		{path: "/run", data: "mode=0755,size=64m"},
		{path: "/tmp", data: "mode=1777,size=256m"},
	} {
		target, err := ensureMountTarget(root, spec.path)
		if err != nil {
			// Runtime paths may be symlink aliases on some distributions.
			if errors.Is(err, errSymlinkMountPath) {
				continue
			}
			return fmt.Errorf("prepare isolated runtime path %s: %w", spec.path, err)
		}
		if err := mountTmpfs(target, spec.data, syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC); err != nil {
			return fmt.Errorf("mount private runtime tmpfs at %s: %w", spec.path, err)
		}
	}
	return nil
}

func setupVarTmpMount(root string) error {
	target, err := ensureMountTarget(root, "/var/tmp")
	if err != nil {
		if errors.Is(err, errSymlinkMountPath) {
			return nil
		}
		return fmt.Errorf("prepare isolated runtime path /var/tmp: %w", err)
	}
	if err := mountTmpfs(target, "mode=1777,size=64m", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC); err != nil {
		return fmt.Errorf("mount private runtime tmpfs at /var/tmp: %w", err)
	}
	return nil
}

func createDeviceNode(devRoot, name string, major, minor uint32) error {
	path := filepath.Join(devRoot, name)
	mode := uint32(syscall.S_IFCHR | 0666)
	major64, minor64 := uint64(major), uint64(minor)
	device := (major64&0xfff)<<8 | (minor64 & 0xff) | (minor64&^0xff)<<12 | (major64&^0xfff)<<32
	if err := syscall.Mknod(path, mode, int(device)); err != nil {
		return fmt.Errorf("create private /dev/%s node (CAP_MKNOD is required): %w", name, err)
	}
	return nil
}

func mountTmpfs(target, options string, flags uintptr) error {
	return syscall.Mount("tmpfs", target, "tmpfs", flags, options)
}

func mountBindReadOnly(source, target, mountpoint string, plan []plannedMount) error {
	if err := syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return err
	}
	if err := remountReadOnly(target); err != nil {
		return err
	}
	for _, entry := range plan {
		child := entry.Mount.MountPoint
		if child == mountpoint || !isPathWithin(child, mountpoint) {
			continue
		}
		childTarget := rootedPath(targetRootFromTarget(target, mountpoint), child)
		if err := remountReadOnly(childTarget); err != nil && !errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("make nested mount %q read-only: %w", child, err)
		}
	}
	return nil
}

func mountFileReadOnly(source, target string) error {
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return err
	}
	return syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
}

func targetRootFromTarget(target, mountpoint string) string {
	relative := strings.TrimPrefix(cleanMountPoint(mountpoint), "/")
	if relative == "" {
		return target
	}
	return strings.TrimSuffix(target, string(filepath.Separator)+relative)
}

func remountReadOnly(target string) error {
	return syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
}

var errSymlinkMountPath = errors.New("mount path contains a symlink")

func ensureMountTarget(root, mountpoint string) (string, error) {
	clean := cleanMountPoint(mountpoint)
	target := rootedPath(root, clean)
	if clean == "/" {
		return target, nil
	}
	parent, err := ensureMountParent(root, clean)
	if err != nil {
		return "", err
	}
	current := filepath.Join(parent, filepath.Base(clean))
	info, err := os.Lstat(current)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(current, 0755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err = os.Lstat(current)
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s", errSymlinkMountPath, current)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("mount target %s is not a directory", current)
	}
	return target, nil
}

func ensureMountParent(root, mountpoint string) (string, error) {
	clean := cleanMountPoint(mountpoint)
	if clean == "/" {
		return root, nil
	}
	parentPath := filepath.Dir(clean)
	current := root
	if parentPath == "/" {
		return current, nil
	}
	for _, component := range strings.Split(strings.TrimPrefix(parentPath, "/"), "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("unsafe mount path %q", mountpoint)
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s", errSymlinkMountPath, current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("mount path component %s is not a directory", current)
		}
	}
	return current, nil
}

func ensureMountFileTarget(root, mountpoint string, mode os.FileMode) (string, error) {
	clean := cleanMountPoint(mountpoint)
	if clean == "/" {
		return "", errors.New("filesystem root cannot be a file mountpoint")
	}
	parent, err := ensureMountParent(root, clean)
	if err != nil {
		return "", err
	}
	target := filepath.Join(parent, filepath.Base(clean))
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if createErr != nil {
			return "", createErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", closeErr
		}
		info, err = os.Lstat(target)
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s", errSymlinkMountPath, target)
	}
	if info.IsDir() {
		return "", fmt.Errorf("file mount target %s is a directory", target)
	}
	return target, nil
}

func rootedPath(root, mountpoint string) string {
	clean := cleanMountPoint(mountpoint)
	if clean == "/" {
		return root
	}
	return filepath.Join(root, strings.TrimPrefix(clean, "/"))
}

func shouldSkipPlannedMount(mergedRoot, mountpoint string) bool {
	if isExplicitlyReplacedPath(mountpoint) {
		return true
	}
	// A mount through a symlink alias is already represented by the resolved
	// target (for example /var/run -> /run). Do not follow it in the old root.
	return pathHasSymlinkComponent(mergedRoot, mountpoint)
}

func effectiveSkippedPolicy(mountpoint string, original mountPolicy, plan []plannedMount) mountPolicy {
	for _, root := range []string{"/dev/shm", "/dev/mqueue", "/run", "/tmp", "/var/tmp"} {
		if isPathWithin(mountpoint, root) {
			return policyRuntime
		}
	}
	if isPathWithin(mountpoint, "/proc") {
		return policyProc
	}
	if isPathWithin(mountpoint, "/dev") {
		return policyDevices
	}
	if isExplicitlyReplacedPath(mountpoint) {
		return policyRuntime
	}
	for _, entry := range plan {
		if entry.Mount.MountPoint != mountpoint && isPathWithin(mountpoint, entry.Mount.MountPoint) {
			if entry.Policy == policyReadOnly || entry.Policy == policyKernelMask {
				return entry.Policy
			}
		}
	}
	if original == policyProc || original == policyDevices || original == policyRuntime || original == policySysfs {
		return original
	}
	return policyRuntimeAlias
}

func hideLayerDirectory(layerDir, mergedRoot string) error {
	path := cleanMountPoint(layerDir)
	for _, isolated := range []string{"/proc", "/dev", "/run", "/tmp", "/var/tmp", "/dev/shm"} {
		if isPathWithin(path, isolated) {
			return nil // setupCoreMounts already replaced this subtree.
		}
	}
	if path == "/" {
		return errors.New("layer directory cannot be the filesystem root")
	}
	target, err := ensureMountTarget(mergedRoot, path)
	if err != nil {
		return fmt.Errorf("hide layer directory from sandbox: %w", err)
	}
	if err := mountTmpfs(target, "mode=0700,size=1m", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC); err != nil {
		return fmt.Errorf("hide layer directory %s from sandbox: %w", path, err)
	}
	return nil
}

func appendExplicitManifestMounts(manifest *layerManifest) {
	known := make(map[string]bool, len(manifest.Mounts))
	for _, mount := range manifest.Mounts {
		known[mount.MountPoint] = true
	}
	for _, spec := range []manifestMount{
		{MountPoint: "/proc", Filesystem: "proc", Policy: policyProc},
		{MountPoint: "/sys", Filesystem: "sysfs", Policy: policySysfs},
		{MountPoint: "/dev", Filesystem: "tmpfs", Policy: policyDevices},
		{MountPoint: "/dev/pts", Filesystem: "devpts", Policy: policyDevices},
		{MountPoint: "/dev/shm", Filesystem: "tmpfs", Policy: policyRuntime},
		{MountPoint: "/dev/mqueue", Filesystem: "tmpfs", Policy: policyRuntime},
		{MountPoint: "/run", Filesystem: "tmpfs", Policy: policyRuntime},
		{MountPoint: "/tmp", Filesystem: "tmpfs", Policy: policyRuntime},
		{MountPoint: "/var/tmp", Filesystem: "tmpfs", Policy: policyRuntime},
	} {
		if !known[spec.MountPoint] {
			manifest.Mounts = append(manifest.Mounts, spec)
		}
	}
	for i := range manifest.Mounts {
		if manifest.Mounts[i].MountPoint == "" {
			manifest.Mounts[i].MountPoint = "/"
		}
	}
	sort.Slice(manifest.Mounts, func(i, j int) bool {
		if manifest.Mounts[i].MountPoint != manifest.Mounts[j].MountPoint {
			return manifest.Mounts[i].MountPoint < manifest.Mounts[j].MountPoint
		}
		return manifest.Mounts[i].ID < manifest.Mounts[j].ID
	})
}

func writeManifest(layerDir string, manifest layerManifest) error {
	file, err := os.OpenFile(filepath.Join(layerDir, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create layer manifest: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		_ = file.Close()
		return fmt.Errorf("write layer manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close layer manifest: %w", err)
	}
	return nil
}

func pivotInto(newRoot string) error {
	if _, err := ensureMountTarget(newRoot, "/.oldroot"); err != nil {
		return fmt.Errorf("prepare old root mount point: %w", err)
	}
	if err := syscall.Chdir(newRoot); err != nil {
		return fmt.Errorf("enter composed root: %w", err)
	}
	newRootArg, err := syscall.BytePtrFromString(".")
	if err != nil {
		return err
	}
	oldRootArg, err := syscall.BytePtrFromString(".oldroot")
	if err != nil {
		return err
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_PIVOT_ROOT, uintptr(unsafe.Pointer(newRootArg)), uintptr(unsafe.Pointer(oldRootArg)), 0)
	if errno != 0 {
		return fmt.Errorf("pivot_root into OverlayFS tree: %w", errno)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("change directory after pivot_root: %w", err)
	}
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	_ = os.Remove("/.oldroot")
	return nil
}
