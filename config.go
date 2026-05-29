// config.go
package main

import (
	"fmt"
	"os"
	"strconv"

	toml "github.com/pelletier/go-toml/v2"
)

// validBedrockRegions lists AWS regions where Bedrock is available for Claude models.
var validBedrockRegions = map[string]bool{
	"us-east-1": true,
	"us-east-2": true,
	"us-west-2": true,
}

// LokiConfig holds configuration for Loki log export
type LokiConfig struct {
	Enabled      bool   `toml:"enabled"`
	URL          string `toml:"url"`         // Full push endpoint URL, e.g., http://loki.example.com:3100/loki/api/v1/push
	AuthToken    string `toml:"auth_token"`  // Bearer token for auth (optional)
	BatchSize    int    `toml:"batch_size"`  // Number of entries per batch
	BatchWaitStr string `toml:"batch_wait"`  // Duration string for batch timeout
	RetryMax     int    `toml:"retry_max"`   // Maximum retry attempts
	UseGzip      bool   `toml:"use_gzip"`    // Enable gzip compression
	Environment  string `toml:"environment"` // Environment label (development, staging, production)
}

// APITokenSubstitutionConfig configures the opt-in API token substitution feature.
type APITokenSubstitutionConfig struct {
	Enabled     bool   `toml:"enabled"`
	Command     string `toml:"command"`    // single executable path (NOT a shell line); stdin receives the JSON context, stdout is the real key; use a wrapper script if arguments are needed
	CacheTTLStr string `toml:"cache_ttl"`  // duration string, e.g. "5m"
	CacheSize   int    `toml:"cache_size"` // max cached entries (oldest evicted past this)
	TimeoutStr  string `toml:"timeout"`    // per-resolve duration string, e.g. "2s"
}

type Config struct {
	Port                 int                        `toml:"port"`
	LogDir               string                     `toml:"log_dir"`
	BedrockRegion        string                     `toml:"bedrock_region"` // AWS region for Bedrock (empty = disabled)
	ServiceMode          bool                       `toml:"-"`              // CLI-only, not persisted in config file
	SetupShell           bool                       `toml:"-"`              // CLI-only, not persisted in config file
	Env                  bool                       `toml:"-"`              // CLI-only, not persisted in config file
	Setup                bool                       `toml:"-"`              // CLI-only, not persisted in config file
	Uninstall            bool                       `toml:"-"`              // CLI-only, not persisted in config file
	Status               bool                       `toml:"-"`              // CLI-only, not persisted in config file
	Explore              bool                       `toml:"-"`              // CLI-only, not persisted in config file
	ExplorePort          int                        `toml:"explore_port"`
	Loki                 LokiConfig                 `toml:"loki"`
	ListenHost           string                     `toml:"listen_host"`
	APITokenSubstitution APITokenSubstitutionConfig `toml:"api_token_substitution"`
}

func DefaultConfig() Config {
	return Config{
		Port:   0,
		LogDir: "./logs",
		Loki: LokiConfig{
			Enabled:      false,
			BatchSize:    1000,
			BatchWaitStr: "5s",
			RetryMax:     5,
			UseGzip:      true,
			Environment:  "development",
		},
		ListenHost: "localhost",
		APITokenSubstitution: APITokenSubstitutionConfig{
			Enabled:     false,
			CacheTTLStr: "5m",
			CacheSize:   10000,
			TimeoutStr:  "2s",
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

// ValidateBedrockRegion returns an error if the region is non-empty and not a
// known Bedrock-supported region.
func ValidateBedrockRegion(region string) error {
	if region == "" {
		return nil
	}
	if !validBedrockRegions[region] {
		return fmt.Errorf("unsupported Bedrock region %q (valid: us-east-1, us-east-2, us-west-2)", region)
	}
	return nil
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
	if region := os.Getenv("BEDROCK_REGION"); region != "" {
		cfg.BedrockRegion = region
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

	if host := os.Getenv("LLM_PROXY_LISTEN_HOST"); host != "" {
		cfg.ListenHost = host
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_ENABLED"); v != "" {
		cfg.APITokenSubstitution.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_COMMAND"); v != "" {
		cfg.APITokenSubstitution.Command = v
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_CACHE_TTL"); v != "" {
		cfg.APITokenSubstitution.CacheTTLStr = v
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.APITokenSubstitution.CacheSize = n
		}
	}
	if v := os.Getenv("LLM_PROXY_API_TOKEN_SUBSTITUTION_TIMEOUT"); v != "" {
		cfg.APITokenSubstitution.TimeoutStr = v
	}

	return cfg
}

func LoadConfig(configPath string) (Config, error) {
	cfg := DefaultConfig()

	// Auto-discover config file if not specified
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			defaultPath := home + "/.config/llm-proxy/config.toml"
			if _, err := os.Stat(defaultPath); err == nil {
				configPath = defaultPath
			}
		}
	}

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
