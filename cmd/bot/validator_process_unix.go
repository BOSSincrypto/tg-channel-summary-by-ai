//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

const maxUnixSignalPID = int(^uint32(0) >> 1)

func validatorProcessAlive(pid int) bool {
	if pid <= 0 || pid > maxUnixSignalPID {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
