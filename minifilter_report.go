//go:build !linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vfs-sandbox/internal/minicow"
)

type minifilterReport struct {
	Version int              `json:"version"`
	Changes []minicow.Change `json:"changes"`
}

func writeMinifilterReport(layer string, changes []minicow.Change) error {
	data, err := json.MarshalIndent(minifilterReport{1, changes}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(layer, "minifilter-changes.json"), append(data, '\n'), 0600)
}
func diffMinifilter(layer string) int {
	data, err := os.ReadFile(filepath.Join(layer, "minifilter-changes.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: Minifilter change report unavailable:", err)
		return 2
	}
	var report minifilterReport
	if err = json.Unmarshal(data, &report); err != nil || report.Version != 1 {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: invalid Minifilter change report")
		return 2
	}
	for _, change := range report.Changes {
		if !validMinifilterChange(change) {
			fmt.Fprintln(os.Stderr, "vfs-sandbox: invalid Minifilter change path or kind")
			return 2
		}
		parts := strings.SplitN(change.Path, "/", 2)
		path := strings.ToUpper(parts[0]) + `:\`
		if len(parts) == 2 {
			path += strings.ReplaceAll(parts[1], "/", `\`)
		}
		fmt.Printf("%-10s %s\n", change.Kind, path)
	}
	if len(report.Changes) == 0 {
		fmt.Println("No filesystem changes were recorded.")
	}
	return 0
}
func validMinifilterChange(change minicow.Change) bool {
	if change.Kind != "created" && change.Kind != "changed" && change.Kind != "deleted" {
		return false
	}
	if !filepath.IsLocal(filepath.FromSlash(change.Path)) {
		return false
	}
	parts := strings.Split(change.Path, "/")
	if len(parts[0]) != 1 || strings.ToUpper(parts[0]) < "A" || strings.ToUpper(parts[0]) > "Z" {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\:*?`) {
			return false
		}
	}
	return true
}
func exportMinifilterArtifacts(layer string, changes []minicow.Change, requested, defaultParent string) (path string, count int, size int64, err error) {
	path, err = createPortableArtifactDirectory(layer, requested, defaultParent)
	if err != nil {
		return
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(path)
		}
	}()
	for _, change := range changes {
		if !validMinifilterChange(change) {
			err = fmt.Errorf("invalid change path %q", change.Path)
			return
		}
		if change.Kind == "deleted" || change.Directory {
			continue
		}
		relative := filepath.FromSlash(change.Path)
		var parent string
		parent, err = safePortableTreePath(layer, filepath.Join("minifilter-upper", filepath.Dir(relative)))
		if err != nil {
			return
		}
		source := filepath.Join(parent, filepath.Base(relative))
		var info os.FileInfo
		info, err = os.Lstat(source)
		if err != nil {
			return
		}
		if !info.Mode().IsRegular() {
			err = fmt.Errorf("non-regular Minifilter artifact %q", source)
			return
		}
		var written int64
		written, err = copyPortableArtifactFile(source, filepath.Join(path, "drives", relative), info)
		if err != nil {
			return
		}
		count++
		size += written
	}
	complete = true
	return
}
