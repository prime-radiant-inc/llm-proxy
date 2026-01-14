// config.go
package main

import (
	"os"
	"strconv"

	toml "github.com/pelletier/go-toml/v2"
)

type Config struct {
	Port        int    `toml:"port"`
	LogDir      string `toml:"log_dir"`
	ServiceMode bool   `toml:"-"` // CLI-only, not persisted in config file
}

func DefaultConfig() Config {
	return Config{
		Port:   8080,
		LogDir: "./logs",
	}
}

func LoadConfigFromTOML(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadConfigFromEnv(cfg Config) Config {
	if port := os.Getenv("LLM_PROXY_PORT"); port != "" {
		if p, err := strconv.Atoi(port); err == nil {
			cfg.Port = p
		}
	}
	if logDir := os.Getenv("LLM_PROXY_LOG_DIR"); logDir != "" {
		cfg.LogDir = logDir
	}
	return cfg
}

func LoadConfig(configPath string) (Config, error) {
	cfg := DefaultConfig()

	// Try to load from TOML file if it exists
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			cfg, err = LoadConfigFromTOML(data)
			if err != nil {
				return Config{}, err
			}
		}
	}

	// Override with environment variables
	cfg = LoadConfigFromEnv(cfg)

	return cfg, nil
}
