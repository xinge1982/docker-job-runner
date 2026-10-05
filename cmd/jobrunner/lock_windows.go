package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// Lock the first byte of the dedicated lock file. Windows allows this range
// even when the file is empty. Without FAIL_IMMEDIATELY, acquisition blocks.
func lockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped)
}

func unlockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
