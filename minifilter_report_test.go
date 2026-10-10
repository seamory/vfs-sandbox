//go:build !linux

package main

import (
	"testing"
	"vfs-sandbox/internal/minicow"
)

func TestMinifilterReportRejectsUnsafePaths(t *testing.T) {
	for _, path := range []string{"../host", "c/../host", "c/a:stream", "/c/a", "c//a", "c/a\\b", "cc/a", "c/./a"} {
		if validMinifilterChange(minicow.Change{Path: path, Kind: "changed"}) {
			t.Fatal("accepted", path)
		}
	}
	for _, path := range []string{"c", "c/windows/file.txt", "d/project/世界.txt"} {
		if !validMinifilterChange(minicow.Change{Path: path, Kind: "changed"}) {
			t.Fatal("rejected", path)
		}
	}
	if validMinifilterChange(minicow.Change{Path: "c/file", Kind: "arbitrary"}) {
		t.Fatal("accepted invalid change kind")
	}
}
