//go:build !linux && !darwin

package main

func copyPortableWorkspace(source, destination string) error {
	return copyPortableTree(source, destination)
}
