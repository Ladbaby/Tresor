package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Build-time metadata (injected via -ldflags or go generate)
//
// These defaults are overridden at build time. For CI builds, use -ldflags.
// For local dev builds, run `go generate ./cmd` first (see generate.sh).
var (
	Version   = "dev"
	BuildTime = "dev"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "tresor",
	Short: "A single-binary LLM gateway for switching providers at scale with one click",
	Long: `Tresor is a single-binary LLM gateway for switching providers at scale with one click.

It sits between client applications and LLM providers, transforming
requests and responses via a configurable plugin pipeline.`,
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Fprintf(os.Stderr, "Usage: tresor <command> [flags]\n\nRun 'tresor --help' for available commands.\n")
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "path to YAML config file (default: ./config.yaml or $HOME/.tresor.yaml)")
}
