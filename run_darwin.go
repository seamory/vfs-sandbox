//go:build darwin

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func run(args []string) int {
	if len(args) > 0 && args[0] == "diff" {
		return runPortableDiff(args[1:])
	}
	invocation, code, done := parsePortableInvocation(args, printUsage)
	if done {
		return code
	}
	layerDir, removeLayer, err := preparePortableLayer(invocation.LayerDir, invocation.KeepLayer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 2
	}
	if removeLayer {
		defer func() {
			if removeLayer {
				_ = os.RemoveAll(layerDir)
			}
		}()
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: get working directory: %v\n", err)
		return 125
	}
	workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: resolve working directory: %v\n", err)
		return 125
	}
	if err := validatePortableLayerLocation(layerDir, workingDirectory); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 2
	}
	if invocation.SaveArtifacts {
		if err := validatePortableArtifactDirectory(layerDir, invocation.ArtifactDir, workingDirectory); err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: artifact directory: %v\n", err)
			return 2
		}
	}
	workspace := filepath.Join(layerDir, "workspace")
	snapshot, err := portableWorkspace(workingDirectory, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 125
	}
	for _, directory := range []string{"tmp", "home", "cache", "config", "state"} {
		if err := os.MkdirAll(filepath.Join(layerDir, directory), 0700); err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: create private %s directory: %v\n", directory, err)
			return 125
		}
	}
	additionalSnapshots := make(map[string]map[string]string)
	for _, directory := range []string{"tmp", "home", "cache", "config", "state"} {
		baseline, err := snapshotPortableTree(filepath.Join(layerDir, directory))
		if err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: record private %s directory: %v\n", directory, err)
			return 125
		}
		additionalSnapshots[directory] = baseline
	}
	manifest := portableManifest{
		Version:             1,
		Platform:            "darwin-seatbelt",
		CreatedAt:           time.Now().UTC(),
		SourceDirectory:     workingDirectory,
		Workspace:           "workspace",
		Snapshot:            snapshot,
		AdditionalSnapshots: additionalSnapshots,
		Command:             invocation.Command,
		Arguments:           invocation.Args,
	}
	if err := writePortableManifest(layerDir, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 125
	}
	profilePath := filepath.Join(layerDir, "profile.sb")
	profile, err := darwinSandboxProfile(layerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: prepare Seatbelt profile: %v\n", err)
		return 125
	}
	if err := os.WriteFile(profilePath, []byte(profile), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write Seatbelt profile: %v\n", err)
		return 125
	}
	sandboxExec, err := exec.LookPath("sandbox-exec")
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: macOS sandbox-exec is unavailable")
		return 125
	}
	command := exec.Command(sandboxExec, append([]string{"-f", profilePath, invocation.Command}, invocation.Args...)...)
	command.Dir = workspace
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"HOME":            filepath.Join(layerDir, "home"),
		"TMPDIR":          filepath.Join(layerDir, "tmp"),
		"TMP":             filepath.Join(layerDir, "tmp"),
		"TEMP":            filepath.Join(layerDir, "tmp"),
		"XDG_CACHE_HOME":  filepath.Join(layerDir, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(layerDir, "config"),
		"XDG_STATE_HOME":  filepath.Join(layerDir, "state"),
	})
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = command.Run()
	if invocation.SaveArtifacts {
		artifactPath, artifactCount, artifactBytes, artifactErr := exportPortableArtifacts(layerDir, manifest, invocation.ArtifactDir, workingDirectory)
		if artifactErr != nil {
			removeLayer = false
			portableLayerMessage(true, layerDir)
			fmt.Fprintf(os.Stderr, "vfs-sandbox: export runtime artifacts: %v\n", artifactErr)
			return 125
		}
		portableArtifactsMessage(artifactPath, artifactCount, artifactBytes)
	}
	portableLayerMessage(invocation.KeepLayer || invocation.LayerDir != "", layerDir)
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "vfs-sandbox: run command under macOS Seatbelt: %v\n", err)
		return 125
	}
	return 0
}

func darwinSandboxProfile(layerDir string) (string, error) {
	canonicalLayer, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return "", err
	}
	var profile strings.Builder
	profile.WriteString("(version 1)\n(deny default)\n")
	profile.WriteString("(allow file-read*)\n(allow file-read-metadata)\n")
	for _, directory := range []string{"workspace", "tmp", "home", "cache", "config", "state"} {
		path, err := filepath.EvalSymlinks(filepath.Join(canonicalLayer, directory))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&profile, "(allow file-write* (subpath %s))\n", quoteSBPL(path))
	}
	profile.WriteString(`(allow file-write* (literal "/dev/null") (literal "/dev/tty"))
(allow file-ioctl)
(allow process-exec*)
(allow process-fork)
(allow process-info*)
(allow signal (target same-sandbox))
(allow sysctl-read)
(allow mach-lookup)
(allow mach-host*)
(allow network*)
`)
	return profile.String(), nil
}

func quoteSBPL(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, "\r", `\r`)
	return `"` + value + `"`
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  vfs-sandbox [--layer-dir DIR | --keep-layer] [--save-artifacts [--artifact-dir DIR]] -- COMMAND [ARG...]
  vfs-sandbox diff LAYER_DIR

Run COMMAND under the macOS Seatbelt file-write policy. The current working
directory is copied into a private workspace; writes there and under private
temporary and home directories are retained in the layer. Writes elsewhere
are denied by macOS.

Options:
  --layer-dir DIR  create a retained layer at this new directory
  --keep-layer     retain the automatically created layer and print its path
  --save-artifacts  save changed files after the command exits (default: off)
  --artifact-dir DIR  choose a new artifact directory (requires --save-artifacts)
  -h, --help       show this help

Examples:
  vfs-sandbox --keep-layer -- /bin/sh
  vfs-sandbox diff /path/to/layer`)
}
