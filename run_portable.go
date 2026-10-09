//go:build !linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type portableInvocation struct {
	LayerDir      string
	KeepLayer     bool
	SaveArtifacts bool
	ArtifactDir   string
	Command       string
	Args          []string
}

type portableManifest struct {
	Version             int                          `json:"version"`
	Platform            string                       `json:"platform"`
	CreatedAt           time.Time                    `json:"created_at"`
	SourceDirectory     string                       `json:"source_directory"`
	Workspace           string                       `json:"workspace"`
	Snapshot            map[string]string            `json:"snapshot"`
	AdditionalSnapshots map[string]map[string]string `json:"additional_snapshots,omitempty"`
	Command             string                       `json:"command"`
	Arguments           []string                     `json:"arguments,omitempty"`
}

func parsePortableInvocation(args []string, usage func(io.Writer)) (portableInvocation, int, bool) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		usage(os.Stdout)
		return portableInvocation{}, 0, true
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
		usage(os.Stderr)
		return portableInvocation{}, 2, true
	}
	flags := flag.NewFlagSet("vfs-sandbox", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	layerDir := flags.String("layer-dir", "", "create and retain the writable layer at this new directory")
	keepLayer := flags.Bool("keep-layer", false, "retain the automatically created writable layer")
	saveArtifacts := flags.Bool("save-artifacts", false, "save changed files after the command exits")
	artifactDir := flags.String("artifact-dir", "", "choose the artifact directory when --save-artifacts is enabled")
	if err := flags.Parse(args[:separator]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return portableInvocation{}, 0, true
		}
		return portableInvocation{}, 2, true
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "sandbox options must appear before --")
		return portableInvocation{}, 2, true
	}
	if *layerDir != "" && *keepLayer {
		fmt.Fprintln(os.Stderr, "--layer-dir and --keep-layer cannot be used together")
		return portableInvocation{}, 2, true
	}
	if *artifactDir != "" && !*saveArtifacts {
		fmt.Fprintln(os.Stderr, "--artifact-dir requires --save-artifacts")
		return portableInvocation{}, 2, true
	}
	return portableInvocation{
		LayerDir:      *layerDir,
		KeepLayer:     *keepLayer,
		SaveArtifacts: *saveArtifacts,
		ArtifactDir:   *artifactDir,
		Command:       args[separator+1],
		Args:          append([]string(nil), args[separator+2:]...),
	}, 0, false
}

func preparePortableLayer(requested string, retain bool) (path string, remove bool, err error) {
	if requested != "" {
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
		return path, false, nil
	}
	path, err = os.MkdirTemp(os.TempDir(), "vfs-sandbox-")
	if err != nil {
		return "", false, fmt.Errorf("create temporary layer directory: %w", err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		_ = os.RemoveAll(path)
		return "", false, fmt.Errorf("protect temporary layer directory: %w", err)
	}
	createdPath := path
	path, err = filepath.EvalSymlinks(createdPath)
	if err != nil {
		_ = os.RemoveAll(createdPath)
		return "", false, fmt.Errorf("resolve temporary layer directory: %w", err)
	}
	return path, !retain, nil
}

func portableWorkspace(source, destination string) (map[string]string, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("working directory %q is not a directory", source)
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return nil, fmt.Errorf("create private workspace copy: %w", err)
	}
	snapshot, err := snapshotPortableTree(source)
	if err != nil {
		return nil, fmt.Errorf("record working directory: %w", err)
	}
	if err := copyPortableWorkspace(source, destination); err != nil {
		return nil, fmt.Errorf("copy working directory into private workspace: %w", err)
	}
	return snapshot, nil
}

func copyPortableTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		inErr := in.Close()
		outErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if inErr != nil {
			return inErr
		}
		if outErr != nil {
			return outErr
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}

func snapshotPortableTree(root string) (map[string]string, error) {
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[key] = "symlink:" + link
			return nil
		}
		if info.IsDir() {
			if key == "." {
				snapshot[key] = "directory"
			} else {
				snapshot[key] = fmt.Sprintf("directory:%o", info.Mode().Perm())
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		snapshot[key] = fmt.Sprintf("file:%o:%d:%s", info.Mode().Perm(), info.ModTime().UnixNano(), hex.EncodeToString(hash.Sum(nil)))
		return nil
	})
	return snapshot, err
}

func writePortableManifest(layerDir string, manifest portableManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode layer manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layerDir, "manifest.json"), append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write layer manifest: %w", err)
	}
	return nil
}

func runPortableDiff(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: vfs-sandbox diff LAYER_DIR")
		return 2
	}
	layerDir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: resolve layer directory: %v\n", err)
		return 2
	}
	data, err := os.ReadFile(filepath.Join(layerDir, "manifest.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: read manifest: %v\n", err)
		return 2
	}
	var manifest portableManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: decode manifest: %v\n", err)
		return 2
	}
	if manifest.Version != 1 || manifest.Workspace != "workspace" {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: unsupported manifest version or workspace path\n")
		return 2
	}
	changes := 0
	workspaceRoot, err := safePortableTreePath(layerDir, manifest.Workspace)
	workspaceCurrent := map[string]string{}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "vfs-sandbox diff: resolve workspace: %v\n", err)
		return 2
	}
	if err == nil {
		workspaceCurrent, err = snapshotPortableTree(workspaceRoot)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "vfs-sandbox diff: scan workspace: %v\n", err)
			return 2
		}
	}
	changes += printPortableChanges(manifest.Snapshot, workspaceCurrent, func(path string) string {
		return portableDisplayPath(manifest.SourceDirectory, path)
	})
	additionalNames := make([]string, 0, len(manifest.AdditionalSnapshots))
	for name := range manifest.AdditionalSnapshots {
		additionalNames = append(additionalNames, name)
	}
	sort.Strings(additionalNames)
	for _, name := range additionalNames {
		if !filepath.IsLocal(filepath.FromSlash(name)) || name == "." {
			fmt.Fprintf(os.Stderr, "vfs-sandbox diff: unsafe additional layer path %q\n", name)
			return 2
		}
		root, err := safePortableTreePath(layerDir, name)
		current := map[string]string{}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "vfs-sandbox diff: resolve additional layer %s: %v\n", name, err)
			return 2
		}
		if err == nil {
			current, err = snapshotPortableTree(root)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "vfs-sandbox diff: scan additional layer %s: %v\n", name, err)
				return 2
			}
		}
		changes += printPortableChanges(manifest.AdditionalSnapshots[name], current, func(path string) string {
			return filepath.ToSlash(filepath.Join("sandbox", name, filepath.FromSlash(path)))
		})
	}
	if changes == 0 {
		fmt.Println("No filesystem changes were recorded.")
	}
	return 0
}

type portableArtifactFile struct {
	source      string
	destination string
	info        fs.FileInfo
}

func exportPortableArtifacts(layerDir string, manifest portableManifest, requested, defaultParent string) (string, int, int64, error) {
	files, err := collectPortableArtifacts(layerDir, manifest)
	if err != nil {
		return "", 0, 0, err
	}
	destination, err := createPortableArtifactDirectory(layerDir, requested, defaultParent)
	if err != nil {
		return "", 0, 0, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()

	var totalBytes int64
	for _, file := range files {
		written, err := copyPortableArtifactFile(file.source, filepath.Join(destination, filepath.FromSlash(file.destination)), file.info)
		if err != nil {
			return "", 0, 0, fmt.Errorf("copy %s: %w", file.destination, err)
		}
		totalBytes += written
	}
	complete = true
	return destination, len(files), totalBytes, nil
}

func collectPortableArtifacts(layerDir string, manifest portableManifest) ([]portableArtifactFile, error) {
	var files []portableArtifactFile
	workspaceRoot, err := safePortableTreePath(layerDir, manifest.Workspace)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	if err == nil {
		workspaceFiles, err := collectPortableArtifactTree(workspaceRoot, manifest.Snapshot, "workspace")
		if err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		files = append(files, workspaceFiles...)
	}

	additionalNames := make([]string, 0, len(manifest.AdditionalSnapshots))
	for name := range manifest.AdditionalSnapshots {
		additionalNames = append(additionalNames, name)
	}
	sort.Strings(additionalNames)
	for _, name := range additionalNames {
		if !filepath.IsLocal(filepath.FromSlash(name)) || name == "." {
			return nil, fmt.Errorf("unsafe additional layer path %q", name)
		}
		root, err := safePortableTreePath(layerDir, name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("resolve additional layer %s: %w", name, err)
		}
		additionalFiles, err := collectPortableArtifactTree(root, manifest.AdditionalSnapshots[name], filepath.ToSlash(filepath.Join("sandbox", name)))
		if err != nil {
			return nil, fmt.Errorf("scan additional layer %s: %w", name, err)
		}
		files = append(files, additionalFiles...)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].destination < files[j].destination })
	return files, nil
}

func collectPortableArtifactTree(root string, before map[string]string, prefix string) ([]portableArtifactFile, error) {
	after, err := snapshotPortableTree(root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(after))
	for path := range after {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var files []portableArtifactFile
	for _, relative := range paths {
		current := after[relative]
		if !strings.HasPrefix(current, "file:") || before[relative] == current {
			continue
		}
		cleanRelative := filepath.Clean(filepath.FromSlash(relative))
		if !filepath.IsLocal(cleanRelative) || cleanRelative == "." {
			return nil, fmt.Errorf("unsafe changed path %q", relative)
		}
		source := filepath.Join(root, cleanRelative)
		info, err := os.Lstat(source)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		destination := filepath.ToSlash(filepath.Join(prefix, cleanRelative))
		files = append(files, portableArtifactFile{source: source, destination: destination, info: info})
	}
	return files, nil
}

func createPortableArtifactDirectory(layerDir, requested, defaultParent string) (string, error) {
	if err := validatePortableArtifactDirectory(layerDir, requested, defaultParent); err != nil {
		return "", err
	}
	layerRoot, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return "", fmt.Errorf("resolve layer directory: %w", err)
	}
	if requested == "" {
		parent, err := filepath.Abs(defaultParent)
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
		if portablePathWithin(destination, layerRoot) {
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
		return "", fmt.Errorf("resolve artifact directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return "", fmt.Errorf("resolve artifact directory parent: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if portablePathWithin(destination, layerRoot) {
		return "", errors.New("artifact directory must be outside the retained layer")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return "", fmt.Errorf("create artifact directory %q (it must not already exist): %w", destination, err)
	}
	return destination, nil
}

func validatePortableArtifactDirectory(layerDir, requested, defaultParent string) error {
	layerRoot, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return fmt.Errorf("resolve layer directory: %w", err)
	}
	if requested == "" {
		parent, err := filepath.Abs(defaultParent)
		if err != nil {
			return fmt.Errorf("resolve current directory for artifacts: %w", err)
		}
		parent, err = filepath.EvalSymlinks(parent)
		if err != nil {
			return fmt.Errorf("resolve current directory for artifacts: %w", err)
		}
		if portablePathWithin(parent, layerRoot) {
			return errors.New("artifact directory must be outside the retained layer")
		}
		return nil
	}
	destination, err := filepath.Abs(requested)
	if err != nil {
		return fmt.Errorf("resolve artifact directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return fmt.Errorf("resolve artifact directory parent: %w", err)
	}
	destination = filepath.Join(parent, filepath.Base(destination))
	if portablePathWithin(destination, layerRoot) {
		return errors.New("artifact directory must be outside the retained layer")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("artifact directory %q already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	return nil
}

func copyPortableArtifactFile(sourcePath, destinationPath string, expected fs.FileInfo) (int64, error) {
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
		return 0, errors.New("source file changed during artifact export")
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

func portableArtifactsMessage(path string, fileCount int, totalBytes int64) {
	fmt.Fprintf(os.Stderr, "Exported %d file(s), %d bytes, to %s\n", fileCount, totalBytes, path)
}

func printPortableChanges(before, after map[string]string, displayPath func(string) string) int {
	var paths []string
	for path := range before {
		paths = append(paths, path)
	}
	for path := range after {
		if _, exists := before[path]; !exists {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	changes := 0
	for _, path := range paths {
		oldValue, existed := before[path]
		newValue, exists := after[path]
		kind := ""
		switch {
		case !existed && exists:
			kind = "created"
		case existed && !exists:
			kind = "deleted"
		case oldValue != newValue:
			kind = "changed"
		}
		if kind != "" {
			fmt.Printf("%-10s %s\n", kind, displayPath(path))
			changes++
		}
	}
	return changes
}

func safePortableTreePath(layerDir, relative string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if !filepath.IsLocal(clean) || clean == "." {
		return "", fmt.Errorf("unsafe path %q", relative)
	}
	root, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return "", err
	}
	current := root
	components := strings.Split(clean, string(filepath.Separator))
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
			return "", fmt.Errorf("path contains a symlink at %s", current)
		}
		if index < len(components)-1 && !info.IsDir() {
			return "", fmt.Errorf("path component %s is not a directory", current)
		}
	}
	info, err := os.Stat(current)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %s is not a directory", current)
	}
	return current, nil
}

func portableDisplayPath(root, relative string) string {
	if relative == "." || relative == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(relative))
}

func replaceEnvironment(environment []string, values map[string]string) []string {
	result := make([]string, 0, len(environment)+len(values))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := values[strings.ToUpper(key)]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func portableLayerMessage(keep bool, path string) {
	if keep {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: layer retained at %s\n", path)
	}
}

func validatePortableLayerLocation(layerDir, source string) error {
	layer, err := filepath.EvalSymlinks(layerDir)
	if err != nil {
		return fmt.Errorf("resolve layer directory: %w", err)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	if portablePathWithin(layer, source) {
		return fmt.Errorf("layer directory %q must not be inside the working directory %q", layerDir, source)
	}
	return nil
}

func portablePathWithin(path, root string) bool {
	path, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
