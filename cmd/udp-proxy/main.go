package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/proxy"
)

// Build-time variables injected by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	configPath := flag.String("config", "", "path to YAML configuration file")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("udp-proxy %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "usage: udp-proxy -config <config.yaml>")
		os.Exit(1)
	}

	p, err := proxy.New(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	if err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "proxy error: %v\n", err)
		os.Exit(1)
	}
}
