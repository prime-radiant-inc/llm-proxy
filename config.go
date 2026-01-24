// config.go
package main

import (
	"os"
	"strconv"

	toml "github.com/pelletier/go-toml/v2"
)

// LokiConfig holds configuration for Loki log export
type LokiConfig struct {
	Enabled      bool   `toml:"enabled"`
	URL          string `toml:"url"`          // Full push endpoint URL, e.g., http://loki.example.com:3100/loki/api/v1/push
	AuthToken    string `toml:"auth_token"`   // Bearer token for auth (optional)
	BatchSize    int    `toml:"batch_size"`   // Number of entries per batch
	BatchWaitStr string `toml:"batch_wait"`   // Duration string for batch timeout
	RetryMax     int    `toml:"retry_max"`    // Maximum retry attempts
	UseGzip      bool   `toml:"use_gzip"`     // Enable gzip compression
	Environment  string `toml:"environment"`  // Environment label (development, staging, production)
}

type Config struct {
	Port        int    `toml:"port"`
	LogDir      string `toml:"log_dir"`
	ServiceMode bool   `toml:"-"` // CLI-only, not persisted in config file
	SetupShell  bool   `toml:"-"` // CLI-only, not persisted in config file
	Env         bool   `toml:"-"` // CLI-only, not persisted in config file
	Setup       bool   `toml:"-"` // CLI-only, not persisted in config file
	Uninstall   bool   `toml:"-"` // CLI-only, not persisted in config file
	Status      bool   `toml:"-"` // CLI-only, not persisted in config file
	Explore     bool   `toml:"-"` // CLI-only, not persisted in config file
	ExplorePort int    `toml:"explore_port"`
	Loki        LokiConfig `toml:"loki"`
}

func DefaultConfig() Config {
	return Config{
		Port:   8080,
		LogDir: "./logs",
		Loki: LokiConfig{
			Enabled:      false,
			BatchSize:    1000,
			BatchWaitStr: "5s",
			RetryMax:     5,
			UseGzip:      true,
			Environment:  "development",
		},
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

	// Loki configuration
	if enabled := os.Getenv("LLM_PROXY_LOKI_ENABLED"); enabled != "" {
		cfg.Loki.Enabled = enabled == "true" || enabled == "1"
	}
	if url := os.Getenv("LLM_PROXY_LOKI_URL"); url != "" {
		cfg.Loki.URL = url
	}
	if authToken := os.Getenv("LLM_PROXY_LOKI_AUTH_TOKEN"); authToken != "" {
		cfg.Loki.AuthToken = authToken
	}
	if batchSize := os.Getenv("LLM_PROXY_LOKI_BATCH_SIZE"); batchSize != "" {
		if bs, err := strconv.Atoi(batchSize); err == nil {
			cfg.Loki.BatchSize = bs
		}
	}
	if batchWait := os.Getenv("LLM_PROXY_LOKI_BATCH_WAIT"); batchWait != "" {
		cfg.Loki.BatchWaitStr = batchWait
	}
	if retryMax := os.Getenv("LLM_PROXY_LOKI_RETRY_MAX"); retryMax != "" {
		if rm, err := strconv.Atoi(retryMax); err == nil {
			cfg.Loki.RetryMax = rm
		}
	}
	if useGzip := os.Getenv("LLM_PROXY_LOKI_USE_GZIP"); useGzip != "" {
		cfg.Loki.UseGzip = useGzip == "true" || useGzip == "1"
	}
	if env := os.Getenv("LLM_PROXY_LOKI_ENVIRONMENT"); env != "" {
		cfg.Loki.Environment = env
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
