//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package main

import (
	"fmt"
	"os"
	"runtime"
)

func lockFile(file *os.File) error {
	return fmt.Errorf("job locking is unsupported on %s", runtime.GOOS)
}

func unlockFile(file *os.File) error {
	return fmt.Errorf("job locking is unsupported on %s", runtime.GOOS)
}
