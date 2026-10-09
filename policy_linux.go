//go:build linux

package main

import (
	"path/filepath"
	"sort"
	"strings"
)

type mountPolicy string

const (
	policyOverlay      mountPolicy = "overlay"
	policyOverlayFile  mountPolicy = "overlay-file"
	policyMaskFile     mountPolicy = "masked-special-file"
	policyProc         mountPolicy = "fresh-proc"
	policySysfs        mountPolicy = "fresh-read-only-sysfs"
	policyDevices      mountPolicy = "private-dev-tmpfs"
	policyRuntime      mountPolicy = "private-runtime-tmpfs"
	policyReadOnly     mountPolicy = "shared-read-only"
	policyKernelMask   mountPolicy = "masked-kernel-interface"
	policyRuntimeAlias mountPolicy = "runtime-symlink-alias"
)

type plannedMount struct {
	Mount  mountInfo
	Policy mountPolicy
}

func planMounts(mounts []mountInfo) []plannedMount {
	// Mountinfo can describe stacked mounts at one path. The highest mount ID is
	// the visible mount in that stack; layering each hidden mount would be wrong.
	visible := make(map[string]mountInfo, len(mounts))
	for _, mount := range mounts {
		path := cleanMountPoint(mount.MountPoint)
		mount.MountPoint = path
		previous, exists := visible[path]
		if !exists || mount.ID > previous.ID {
			visible[path] = mount
		}
	}

	result := make([]plannedMount, 0, len(visible))
	for _, mount := range visible {
		result = append(result, plannedMount{Mount: mount, Policy: classifyMount(mount)})
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].Mount.MountPoint, result[j].Mount.MountPoint
		leftDepth, rightDepth := pathDepth(left), pathDepth(right)
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		if left != right {
			return left < right
		}
		return result[i].Mount.ID < result[j].Mount.ID
	})
	return result
}

func classifyMount(mount mountInfo) mountPolicy {
	path := cleanMountPoint(mount.MountPoint)
	switch path {
	case "/proc":
		return policyProc
	case "/sys":
		return policySysfs
	case "/dev":
		return policyDevices
	}
	if isRuntimePath(path) {
		return policyRuntime
	}
	if mount.Filesystem == "cgroup" || mount.Filesystem == "cgroup2" {
		return policyReadOnly
	}
	if isNetworkFilesystem(mount.Filesystem) {
		return policyReadOnly
	}
	if isKernelInterfaceFilesystem(mount.Filesystem) {
		return policyKernelMask
	}
	return policyOverlay
}

func cleanMountPoint(path string) string {
	if path == "" {
		return "/"
	}
	clean := filepath.Clean("/" + strings.TrimLeft(path, "/"))
	return clean
}

func isRuntimePath(path string) bool {
	for _, root := range []string{"/run", "/tmp", "/var/tmp", "/dev/shm"} {
		if isPathWithin(path, root) {
			return true
		}
	}
	return false
}

func isPathWithin(path, root string) bool {
	path, root = cleanMountPoint(path), cleanMountPoint(root)
	if root == "/" {
		return true
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

func pathDepth(path string) int {
	if path == "/" {
		return 0
	}
	return strings.Count(strings.Trim(path, "/"), "/") + 1
}

func isNetworkFilesystem(fs string) bool {
	switch fs {
	case "nfs", "nfs4", "cifs", "smb3", "9p", "virtiofs", "ceph", "afs", "lustre", "davfs", "fuse.sshfs", "fuse.ceph":
		return true
	default:
		return strings.HasPrefix(fs, "fuse.")
	}
}

func isKernelInterfaceFilesystem(fs string) bool {
	switch fs {
	case "proc", "sysfs", "devtmpfs", "devpts", "mqueue", "securityfs", "debugfs", "tracefs", "configfs", "efivarfs", "bpf", "hugetlbfs", "rpc_pipefs", "autofs", "binfmt_misc":
		return true
	default:
		return false
	}
}
