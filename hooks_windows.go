//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// hookCommand runs a command line through cmd.exe without a console window,
// passing the line unchanged so its own quoting works as typed.
func hookCommand(line string) *exec.Cmd {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.Command(shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `cmd.exe /S /C "` + line + `"`,
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	return cmd
}
