//go:build linux

package main

import (
	"testing"
)

func TestParseMountInfoLineUnescapesPaths(t *testing.T) {
	line := `41 25 0:38 /root\040tree /mnt/a\040b rw,nosuid shared:12 - ext4 /dev/sda1 rw,errors=remount-ro`
	mount, err := parseMountInfoLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if mount.ID != 41 || mount.ParentID != 25 {
		t.Fatalf("unexpected mount IDs: got %d parent %d", mount.ID, mount.ParentID)
	}
	if mount.Root != "/root tree" || mount.MountPoint != "/mnt/a b" {
		t.Fatalf("paths were not unescaped: root=%q mountpoint=%q", mount.Root, mount.MountPoint)
	}
	if mount.Filesystem != "ext4" || mount.Source != "/dev/sda1" {
		t.Fatalf("unexpected filesystem source: %s %s", mount.Filesystem, mount.Source)
	}
	if len(mount.Optional) != 1 || mount.Optional[0] != "shared:12" {
		t.Fatalf("unexpected optional fields: %#v", mount.Optional)
	}
}

func TestPlanMountsChoosesVisibleMountAndOrdersParents(t *testing.T) {
	plan := planMounts([]mountInfo{
		{ID: 1, MountPoint: "/", Filesystem: "ext4"},
		{ID: 2, MountPoint: "/data", Filesystem: "ext4"},
		{ID: 3, MountPoint: "/data", Filesystem: "tmpfs"},
		{ID: 4, MountPoint: "/data/cache", Filesystem: "xfs"},
	})
	if len(plan) != 3 {
		t.Fatalf("got %d planned mounts, want 3", len(plan))
	}
	if plan[0].Mount.MountPoint != "/" || plan[1].Mount.MountPoint != "/data" || plan[2].Mount.MountPoint != "/data/cache" {
		t.Fatalf("mounts are not parent-first: %#v", plan)
	}
	if plan[1].Mount.ID != 3 || plan[1].Policy != policyOverlay {
		t.Fatalf("did not select visible mount: %#v", plan[1])
	}
}

func TestClassifyMountPolicies(t *testing.T) {
	cases := []struct {
		mount mountInfo
		want  mountPolicy
	}{
		{mountInfo{MountPoint: "/", Filesystem: "ext4"}, policyOverlay},
		{mountInfo{MountPoint: "/proc", Filesystem: "proc"}, policyProc},
		{mountInfo{MountPoint: "/sys", Filesystem: "sysfs"}, policySysfs},
		{mountInfo{MountPoint: "/dev", Filesystem: "devtmpfs"}, policyDevices},
		{mountInfo{MountPoint: "/run/user/1000", Filesystem: "tmpfs"}, policyRuntime},
		{mountInfo{MountPoint: "/sys/fs/cgroup", Filesystem: "cgroup2"}, policyReadOnly},
		{mountInfo{MountPoint: "/mnt/share", Filesystem: "nfs4"}, policyReadOnly},
		{mountInfo{MountPoint: "/workspace", Filesystem: "virtiofs"}, policyReadOnly},
		{mountInfo{MountPoint: "/sys/kernel/debug", Filesystem: "debugfs"}, policyKernelMask},
		{mountInfo{MountPoint: "/home", Filesystem: "xfs"}, policyOverlay},
	}
	for _, test := range cases {
		if got := classifyMount(test.mount); got != test.want {
			t.Errorf("classifyMount(%q, %q) = %q, want %q", test.mount.MountPoint, test.mount.Filesystem, got, test.want)
		}
	}
}

func TestEffectiveSkippedPolicyKeepsSpecialMountTypesDistinct(t *testing.T) {
	cases := []struct {
		path string
		want mountPolicy
	}{
		{path: "/proc/bus", want: policyProc},
		{path: "/dev/pts", want: policyDevices},
		{path: "/dev/shm", want: policyRuntime},
		{path: "/run/user/1000", want: policyRuntime},
		{path: "/var/tmp/cache", want: policyRuntime},
	}
	for _, test := range cases {
		if got := effectiveSkippedPolicy(test.path, policyOverlay, nil); got != test.want {
			t.Errorf("effectiveSkippedPolicy(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}
