//go:build !linux && !darwin

package commandprovider

import (
	"errors"
	"os/exec"
)

func supportedPlatform() error {
	return errors.New("provider command transport requires Linux or macOS process groups")
}

func prepareCommand(*exec.Cmd) {}

func terminateCommand(*exec.Cmd) error {
	return supportedPlatform()
}
