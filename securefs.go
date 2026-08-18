package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	ownerOnlyDirMode  = 0o700
	ownerOnlyFileMode = 0o600
)

func ensurePrivateDir(path string) error {
	normalized, err := normalizeManagedPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(normalized, ownerOnlyDirMode); err != nil {
		return err
	}
	return chmodPathIfSupported(normalized, ownerOnlyDirMode)
}

func enforceOwnerOnlyFileMode(path string) error {
	normalized, err := normalizeManagedPath(path)
	if err != nil {
		return err
	}
	return chmodPathIfSupported(normalized, ownerOnlyFileMode)
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

func normalizeManagedPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return "", fmt.Errorf("path must not resolve to current directory")
	}
	return cleaned, nil
}

func safeStoragePathComponent(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(trimmed))
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	result := b.String()
	if result == "" || result == "." || result == ".." {
		return "unknown"
	}
	return result
}
