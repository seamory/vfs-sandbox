//go:build darwin

package main

import (
	"fmt"
	"os/exec"
)

func copyPortableWorkspace(source, destination string) error {
	command := exec.Command("/bin/cp", "-cRp", source+"/.", destination)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("cp: %w: %s", err, output)
	}
	return nil
}
