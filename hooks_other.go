//go:build !windows

package main

import "os/exec"

func hookCommand(line string) *exec.Cmd { return exec.Command("/bin/sh", "-c", line) }
