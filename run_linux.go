//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const childMode = "__vfs_sandbox_child"

func run(args []string) int {
	if len(args) > 0 && args[0] == childMode {
		return runChild(args[1:])
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printUsage(os.Stdout)
		return 0
	}
	if args[0] == "diff" {
		return runDiff(args[1:])
	}
	if args[0] == "export" {
		return runExport(args[1:])
	}

	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator == len(args)-1 {
		fmt.Fprintln(os.Stderr, "a command is required after --")
		printUsage(os.Stderr)
		return 2
	}

	flags := flag.NewFlagSet("vfs-sandbox", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	layerDir := flags.String("layer-dir", "", "create and retain the writable layer at this new directory")
	keepLayer := flags.Bool("keep-layer", false, "retain the automatically created writable layer")
	saveArtifacts := flags.Bool("save-artifacts", false, "save changed files after the command exits")
	artifactDir := flags.String("artifact-dir", "", "choose the artifact directory when --save-artifacts is enabled")
	if err := flags.Parse(args[:separator]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(os.Stdout)
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "sandbox options must appear before --")
		return 2
	}
	if *layerDir != "" && *keepLayer {
		fmt.Fprintln(os.Stderr, "--layer-dir and --keep-layer cannot be used together")
		return 2
	}
	if *artifactDir != "" && !*saveArtifacts {
		fmt.Fprintln(os.Stderr, "--artifact-dir requires --save-artifacts")
		return 2
	}

	layerPath, autoCreated, err := prepareLayerDirectory(*layerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 2
	}
	if *saveArtifacts {
		if err := validateExportDirectory(layerPath, *artifactDir); err != nil {
			if autoCreated || *layerDir != "" {
				_ = os.RemoveAll(layerPath)
			}
			fmt.Fprintf(os.Stderr, "vfs-sandbox: artifact directory: %v\n", err)
			return 2
		}
	}
	retain := *layerDir != "" || *keepLayer
	removeLayer := !retain
	defer func() {
		if removeLayer {
			_ = os.RemoveAll(layerPath)
		}
	}()

	executable, err := os.Executable()
	if err != nil {
		if autoCreated {
			_ = os.RemoveAll(layerPath)
		}
		fmt.Fprintf(os.Stderr, "vfs-sandbox: locate launcher: %v\n", err)
		return 2
	}
	commandArgs := append([]string{childMode, layerPath}, args[separator+1:]...)
	cmd := exec.Command(executable, commandArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if err := cmd.Start(); err != nil {
		if autoCreated || *layerDir != "" {
			_ = os.RemoveAll(layerPath)
		}
		fmt.Fprintf(os.Stderr, "vfs-sandbox: start mount-namespace child: %v\n", err)
		fmt.Fprintln(os.Stderr, "creating a mount namespace requires CAP_SYS_ADMIN; run with suitable Linux privileges")
		return 125
	}
	err = cmd.Wait()
	status := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = exitErr.ExitCode()
			if waitStatus, ok := exitErr.Sys().(syscall.WaitStatus); ok && waitStatus.Signaled() {
				status = 128 + int(waitStatus.Signal())
			}
		} else {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: wait for sandbox process: %v\n", err)
			status = 125
		}
	}
	if *keepLayer {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: layer retained at %s\n", layerPath)
	}
	if *saveArtifacts {
		if exportCode := runExportWithOutput([]string{layerPath, *artifactDir}, os.Stderr); exportCode != 0 {
			if removeLayer {
				removeLayer = false
				fmt.Fprintf(os.Stderr, "vfs-sandbox: layer retained at %s because artifact export failed\n", layerPath)
			}
			return 125
		}
	}
	return status
}

func runChild(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: invalid internal launch request")
		return 125
	}
	if err := setupAndExec(args[0], args[1], args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 125
	}
	return 0
}

func prepareLayerDirectory(requested string) (path string, autoCreated bool, err error) {
	if requested == "" {
		mounts, mountErr := readMountInfo("/proc/self/mountinfo")
		if mountErr != nil {
			return "", false, mountErr
		}
		for _, base := range []string{"/dev/shm", "/run", "/tmp", "/var/tmp"} {
			if !hasTmpfsMountAt(mounts, base) {
				continue
			}
			info, statErr := os.Stat(base)
			if statErr != nil || !info.IsDir() {
				continue
			}
			path, err = os.MkdirTemp(base, "vfs-sandbox-")
			if err == nil {
				_ = os.Chmod(path, 0700)
				return path, true, nil
			}
		}
		return "", false, errors.New("no writable tmpfs mount is available under /dev/shm, /run, /tmp, or /var/tmp")
	}

	path, err = filepath.Abs(requested)
	if err != nil {
		return "", false, fmt.Errorf("resolve layer directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", false, fmt.Errorf("resolve parent of layer directory: %w", err)
	}
	path = filepath.Join(parent, filepath.Base(path))
	if err := os.Mkdir(path, 0700); err != nil {
		return "", false, fmt.Errorf("create layer directory %q (it must not already exist): %w", path, err)
	}
	if err := requireTmpfsBacking(path); err != nil {
		_ = os.RemoveAll(path)
		return "", false, err
	}
	return path, false, nil
}

func hasTmpfsMountAt(mounts []mountInfo, path string) bool {
	for _, mount := range mounts {
		if cleanMountPoint(mount.MountPoint) == cleanMountPoint(path) && mount.Filesystem == "tmpfs" {
			return true
		}
	}
	return false
}

func requireTmpfsBacking(path string) error {
	path = cleanMountPoint(path)
	underPrivateRuntime := false
	for _, root := range []string{"/dev/shm", "/run", "/tmp", "/var/tmp"} {
		if isPathWithin(path, root) {
			underPrivateRuntime = true
			break
		}
	}
	if !underPrivateRuntime {
		return fmt.Errorf("layer directory %q must be under /dev/shm, /run, /tmp, or /var/tmp so the sandbox can hide its backing store", path)
	}
	mounts, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	var selected *mountInfo
	for i := range mounts {
		mount := &mounts[i]
		point := cleanMountPoint(mount.MountPoint)
		if isPathWithin(path, point) && (selected == nil || pathDepth(point) > pathDepth(selected.MountPoint)) {
			selected = mount
		}
	}
	if selected == nil || selected.Filesystem != "tmpfs" {
		return fmt.Errorf("layer directory %q must be on a writable tmpfs mount so it stays outside every OverlayFS lower tree", path)
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  vfs-sandbox [--layer-dir DIR | --keep-layer] [--save-artifacts [--artifact-dir DIR]] -- COMMAND [ARG...]
  vfs-sandbox diff LAYER_DIR
  vfs-sandbox export LAYER_DIR [DEST_DIR]

Run COMMAND with a private OverlayFS writable layer in a Linux mount namespace.
Without a retention option, the layer is deleted when COMMAND exits.

Options:
  --layer-dir DIR  create a retained layer at a new directory
  --keep-layer     retain the automatically created layer and print its path
  --save-artifacts  save changed files after the command exits (default: off)
  --artifact-dir DIR  choose a new artifact directory (requires --save-artifacts)
  export           copy changed regular files from a retained layer to a new directory
  -h, --help       show this help

Examples:
  sudo vfs-sandbox -- /bin/sh
  sudo vfs-sandbox --save-artifacts --artifact-dir ./run-output -- /usr/bin/make
  sudo vfs-sandbox --keep-layer -- /usr/bin/make
  sudo vfs-sandbox diff /path/to/layer
  sudo vfs-sandbox export /path/to/layer ./exported-files`)
}

func resolveExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("find command %q inside the sandbox root: %w", name, err)
	}
	return path, nil
}

func execTarget(name string, args []string) error {
	path, err := resolveExecutable(name)
	if err != nil {
		return err
	}
	return syscall.Exec(path, append([]string{name}, args...), os.Environ())
}
