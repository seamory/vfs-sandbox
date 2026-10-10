package minicow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
)

type Change struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Directory bool   `json:"directory,omitempty"`
}

func fingerprint(path string) (string, error) {
	info, err := checked(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "directory", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%o:%d:%s", info.Mode().Perm(), info.ModTime().UnixNano(), hex.EncodeToString(hash.Sum(nil))), nil
}

// Changes records only observed entries; untouched host paths need no scan.
// Deleted lower entries remain represented after their private copy disappears.
func (o *Overlay) Changes() ([]Change, error) {
	keys := map[string]bool{}
	for k := range o.known {
		keys[k] = true
	}
	for k := range o.baseline {
		keys[k] = true
	}
	result := []Change{}
	for k := range keys {
		name := o.names[k]
		if name == "" {
			name = k
		}
		_, upper, err := o.paths(name)
		if err != nil {
			return nil, err
		}
		before := o.baseline[k]
		info, err := checked(upper)
		if errors.Is(err, fs.ErrNotExist) {
			if before != "" {
				result = append(result, Change{name, "deleted", before == "directory"})
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if o.placeholders[k] {
			continue
		}
		after, err := fingerprint(upper)
		if err != nil {
			return nil, err
		}
		if before == after {
			continue
		}
		kind := "changed"
		if before == "" {
			kind = "created"
		}
		result = append(result, Change{name, kind, info.IsDir()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
