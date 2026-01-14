// service.go
package main

import (
	"os"
	"path/filepath"
	"strconv"
)

// DefaultPortfilePath returns the standard location for the portfile.
// This follows XDG conventions: ~/.local/state/llm-proxy/port
func DefaultPortfilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "llm-proxy", "port")
}

// WritePortfile writes the given port number to the specified file.
// It creates parent directories if they don't exist.
func WritePortfile(path string, port int) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(port)), 0644)
}

// ReadPortfile reads and parses a port number from the specified file.
func ReadPortfile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}
