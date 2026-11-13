//go:build linux || darwin

package commandprovider

import (
	"errors"
	"os/exec"
	"syscall"
)

func supportedPlatform() error { return nil }

func prepareCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateCommand(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	// ESRCH is safe: the process group has already exited.
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
