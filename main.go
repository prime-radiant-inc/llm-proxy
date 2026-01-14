// main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

type CLIFlags struct {
	Port       int
	LogDir     string
	ConfigPath string
}

func ParseCLIFlags(args []string) (CLIFlags, error) {
	fs := flag.NewFlagSet("llm-proxy", flag.ContinueOnError)

	var flags CLIFlags
	fs.IntVar(&flags.Port, "port", 0, "Port to listen on")
	fs.StringVar(&flags.LogDir, "log-dir", "", "Directory for log files")
	fs.StringVar(&flags.ConfigPath, "config", "", "Path to config file")

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

	// Setup graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := NewServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating server: %v\n", err)
		os.Exit(1)
	}
	addr := fmt.Sprintf(":%d", cfg.Port)

	// Run shutdown handler in background
	go func() {
		<-ctx.Done()
		log.Println("Shutting down gracefully...")
		srv.Close()
	}()

	log.Printf("Starting llm-proxy on %s", addr)
	log.Printf("Log directory: %s", cfg.LogDir)

	if err := http.ListenAndServe(addr, srv); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
