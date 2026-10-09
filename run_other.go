//go:build !linux && !darwin && !windows

package main

import (
	"fmt"
	"io"
	"os"
)

func run(args []string) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		printUsage(os.Stdout)
		return 0
	}
	if len(args) > 0 && args[0] == "diff" {
		fmt.Fprintln(os.Stderr, "vfs-sandbox diff is unavailable on this platform")
		return 2
	}
	fmt.Fprintln(os.Stderr, "vfs-sandbox is supported on Linux, macOS, and Windows")
	return 2
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  vfs-sandbox [--layer-dir DIR | --keep-layer] -- COMMAND [ARG...]
  vfs-sandbox diff LAYER_DIR

This platform has no configured filesystem sandbox backend.`)
}
