package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Build-time metadata (injected via -ldflags)
var (
	Version   = "unknown"
	BuildTime = "unknown"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "tresor",
	Short: "LLM Gateway — extensible traffic routing and transformation engine",
	Long: `Tresor is an LLM traffic interception and routing engine.
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
