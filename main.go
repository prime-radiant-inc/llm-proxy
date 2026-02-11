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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

type CLIFlags struct {
	Port        int
	LogDir      string
	ConfigPath  string
	ServiceMode bool
	SetupShell  bool
	Env         bool
	Setup       bool
	Uninstall   bool
	Status      bool
	Explore     bool
	ExplorePort int
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
	fs.BoolVar(&flags.Setup, "setup", false, "Full setup: install systemd service, enable, start, and configure shell")
	fs.BoolVar(&flags.Uninstall, "uninstall", false, "Uninstall: stop service, remove service file, remove shell patches, remove portfile")
	fs.BoolVar(&flags.Status, "status", false, "Show proxy status and exit")
	fs.BoolVar(&flags.Explore, "explore", false, "Start log explorer web UI")
	fs.IntVar(&flags.ExplorePort, "explore-port", 8080, "Port for explorer web UI")

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
	if flags.Setup {
		cfg.Setup = true
	}
	if flags.Uninstall {
		cfg.Uninstall = true
	}
	if flags.Status {
		cfg.Status = true
	}
	if flags.Explore {
		cfg.Explore = true
	}
	if flags.ExplorePort != 0 {
		cfg.ExplorePort = flags.ExplorePort
	}
	return cfg
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	cmd.Start()
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

	// Handle --uninstall: remove installation
	if cfg.Uninstall {
		if err := Uninstall(); err != nil {
			log.Fatalf("Uninstall failed: %v", err)
		}
		os.Exit(0)
	}

	// Handle --status: show proxy status and exit
	if cfg.Status {
		Status()
		os.Exit(0)
	}

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

	// Handle --setup: full Linux installation
	if cfg.Setup {
		if err := FullSetup(); err != nil {
			log.Fatalf("Setup failed: %v", err)
		}
		fmt.Println("LLM Proxy installed and started.")
		fmt.Println("Restart your shell to activate.")
		os.Exit(0)
	}

	// Handle --explore: start log explorer
	if cfg.Explore {
		home, _ := os.UserHomeDir()
		logDir := cfg.LogDir
		// Only default to ~/.llm-provider-logs if --log-dir wasn't explicitly set
		if flags.LogDir == "" {
			logDir = filepath.Join(home, ".llm-provider-logs")
		}

		port := cfg.ExplorePort
		if port == 0 {
			port = 8080
		}

		explorer := NewExplorer(logDir)

		url := fmt.Sprintf("http://localhost:%d", port)
		log.Printf("Starting LLM Proxy Explorer on %s", url)

		// Auto-open browser (best effort, don't fail if it doesn't work)
		go func() {
			time.Sleep(100 * time.Millisecond)
			openBrowser(url)
		}()

		addr := fmt.Sprintf(":%d", port)
		if err := http.ListenAndServe(addr, explorer); err != nil {
			log.Fatalf("Explorer server error: %v", err)
		}
		os.Exit(0)
	}

	// Service mode overrides: use dynamic port and default log dir
	if cfg.ServiceMode {
		// Use port 0 for dynamic assignment unless explicitly set via --port or config file
		if flags.Port == 0 && cfg.Port == 0 {
			cfg.Port = 0
		}
		// Use ~/.llm-provider-logs/ unless explicitly set via --log-dir
		if flags.LogDir == "" {
			home, _ := os.UserHomeDir()
			cfg.LogDir = filepath.Join(home, ".llm-provider-logs")
		}
	}

	srv, err := NewServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating server: %v\n", err)
		os.Exit(1)
	}

	// Bind to localhost only — the proxy has no authentication, so binding to
	// all interfaces would allow unauthenticated access if security groups are
	// misconfigured. In ECS awsvpc mode, localhost is shared between containers
	// in the same task, so the PA container can still reach the proxy.
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
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

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Setup graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("Shutting down gracefully...")
		// Allow in-flight streaming requests to complete (Bedrock extended
		// thinking can take 3-5 min TTFB)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown error: %v", err)
		}
		srv.Close()
	}()

	log.Printf("Starting llm-proxy on %s", addr)
	log.Printf("Log directory: %s", cfg.LogDir)
	if cfg.Loki.Enabled {
		log.Printf("Loki export: enabled (%s)", cfg.Loki.URL)
	} else {
		log.Printf("Loki export: disabled")
	}

	if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
