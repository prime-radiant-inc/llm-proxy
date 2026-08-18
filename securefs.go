package main

import (
	"os"
	"runtime"
)

const (
	ownerOnlyDirMode  = 0o700
	ownerOnlyFileMode = 0o600
)

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, ownerOnlyDirMode); err != nil {
		return err
	}
	return chmodPathIfSupported(path, ownerOnlyDirMode)
}

func enforceOwnerOnlyFileMode(path string) error {
	return chmodPathIfSupported(path, ownerOnlyFileMode)
}

func enforceOwnerOnlyOpenFileMode(f *os.File) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return f.Chmod(ownerOnlyFileMode)
}

func chmodPathIfSupported(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return os.Chmod(path, mode)
}
