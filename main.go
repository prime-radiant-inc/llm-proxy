// main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

type CLIFlags struct {
	Port        int
	LogDir      string
	ConfigPath  string
	ServiceMode bool
	SetupShell  bool
	Env         bool
}

func ParseCLIFlags(args []string) (CLIFlags, error) {
	fs := flag.NewFlagSet("llm-proxy", flag.ContinueOnError)

	var flags CLIFlags
	fs.IntVar(&flags.Port, "port", 0, "Port to listen on")
	fs.StringVar(&flags.LogDir, "log-dir", "", "Directory for log files")
	fs.StringVar(&flags.ConfigPath, "config", "", "Path to config file")
	fs.BoolVar(&flags.ServiceMode, "service", false, "Run as background service (dynamic port, write portfile)")
	fs.BoolVar(&flags.SetupShell, "setup-shell", false, "Configure shell integration and exit")
	fs.BoolVar(&flags.Env, "env", false, "Output environment variables for shell eval and exit")

	if err := fs.Parse(args); err != nil {
		return CLIFlags{}, err
	}

	return flags, nil
}

func MergeConfig(cfg Config, flags CLIFlags) Config {
	if flags.Port != 0 {
		cfg.Port = flags.Port
	}
	if flags.LogDir != "" {
		cfg.LogDir = flags.LogDir
	}
	if flags.ServiceMode {
		cfg.ServiceMode = true
	}
	if flags.SetupShell {
		cfg.SetupShell = true
	}
	if flags.Env {
		cfg.Env = true
	}
	return cfg
}

func main() {
	flags, err := ParseCLIFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	cfg, err := LoadConfig(flags.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	cfg = MergeConfig(cfg, flags)

	// Handle --env: output environment variables for shell eval and exit
	if cfg.Env {
		// Read portfile
		port, err := ReadPortfile(DefaultPortfilePath())
		if err != nil {
			// Proxy not configured, output nothing
			os.Exit(0)
		}

		// Health check
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", port))
		if err != nil || resp.StatusCode != 200 {
			// Proxy not running, output nothing
			if resp != nil {
				resp.Body.Close()
			}
			os.Exit(0)
		}
		resp.Body.Close()

		// Output exports
		fmt.Printf("export ANTHROPIC_BASE_URL=\"http://localhost:%d/anthropic/api.anthropic.com\"\n", port)
		fmt.Printf("export OPENAI_BASE_URL=\"http://localhost:%d/openai/api.openai.com\"\n", port)
		os.Exit(0)
	}

	// Handle --setup-shell: configure shell integration and exit
	if cfg.SetupShell {
		if err := PatchAllShells(); err != nil {
			log.Fatalf("Failed to patch shell rc: %v", err)
		}
		fmt.Println("Shell configuration complete. Restart your shell to activate.")
		os.Exit(0)
	}

	// Service mode overrides: use dynamic port and default log dir
	if cfg.ServiceMode {
		// Use port 0 for dynamic assignment unless explicitly set via --port
		if flags.Port == 0 {
			cfg.Port = 0
		}
		// Use ~/.llm-provider-logs/ unless explicitly set via --log-dir
		if flags.LogDir == "" {
			home, _ := os.UserHomeDir()
			cfg.LogDir = filepath.Join(home, ".llm-provider-logs")
		}
	}

	// Setup graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := NewServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating server: %v\n", err)
		os.Exit(1)
	}

	// Create listener (allows us to get actual port for dynamic binding)
	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error binding to %s: %v\n", addr, err)
		os.Exit(1)
	}

	// Get actual port (important for service mode with port 0)
	actualPort := listener.Addr().(*net.TCPAddr).Port

	// In service mode, write portfile
	if cfg.ServiceMode {
		portfilePath := DefaultPortfilePath()
		if err := WritePortfile(portfilePath, actualPort); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing portfile: %v\n", err)
			os.Exit(1)
		}
		log.Printf("Wrote port %d to %s", actualPort, portfilePath)
	}

	// Run shutdown handler in background
	go func() {
		<-ctx.Done()
		log.Println("Shutting down gracefully...")
		srv.Close()
		listener.Close()
	}()

	log.Printf("Starting llm-proxy on :%d", actualPort)
	log.Printf("Log directory: %s", cfg.LogDir)

	if err := http.Serve(listener, srv); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
