//go:build windows

package app

import (
	"os"
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008

func SpawnDetached(argv []string, env []string) error {
	devNull, err := os.OpenFile("NUL", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
		HideWindow:    true,
	}
	return cmd.Start()
}
