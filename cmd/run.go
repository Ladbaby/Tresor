package cmd

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"tresor/internal/api"
	"tresor/internal/config"
	"tresor/internal/engine"
	"tresor/internal/icons"
	"tresor/internal/plugins"
	"tresor/internal/proxy"
	"tresor/internal/store"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the Tresor daemon (HTTP gateway + admin API)",
	Long: `Starts the gateway server using the YAML config file.
The config file is auto-detected (./config.yaml or $HOME/.tresor.yaml)
or can be specified with --config.

Example:
  tresor run
  tresor run --config /path/to/config.yaml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath, _ := cmd.Flags().GetString("config")

		cfg, err := config.Load(cfgPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		return runDaemon(cfg)
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("config", "", "path to YAML config file (default: ./config.yaml or $HOME/.tresor.yaml)")
}

func runDaemon(cfg *config.AppConfig) error {
	// Open store
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	defer s.Close()

	// Load YAML config data into DB (upsert). Falls back to seed defaults
	// if no downstreams/rules/aliases are defined in the config.
	if err := s.LoadConfigData(cfg); err != nil {
		return fmt.Errorf("failed to load config data: %w", err)
	}

	// Build engine
	eng := engine.New(s)

	// Initialize plugin registry and attach to engine
	reg := plugins.NewRegistry()
	eng.SetRegistry(reg)

	// Configure outbound proxy for downstream requests
	eng.SetProxyMode(proxy.Mode(cfg.ProxyMode))

	// Configure inbound proxy request authentication
	eng.SetProxyAuthKeys(cfg.ProxyAPIKeys)

	// Build icon fetcher — resolves model IDs to SVG icons, lazily fetching
	// from a public CDN on first miss and caching to disk beside the DB.
	// Default cache dir is <db_dir>/tresor-icons unless icon_cache_dir is set
	// in the YAML.
	iconCacheDir := cfg.IconCacheDir
	if iconCacheDir == "" {
		iconCacheDir = filepath.Join(filepath.Dir(cfg.DBPath), "tresor-icons")
	}
	iconFetcher, err := icons.NewWithProxyMode(iconCacheDir, proxy.Mode(cfg.ProxyMode))
	if err != nil {
		return fmt.Errorf("init icon fetcher: %w", err)
	}

	// Start the periodic icon-index sync in the background. The cancel
	// func stops the goroutine on daemon shutdown; deferred so it
	// always runs even when we return early via the error path below.
	refreshCtx, stopRefresh := context.WithCancel(context.Background())
	defer stopRefresh()
	iconFetcher.StartPeriodicRefresh(refreshCtx)

	// Initialize request logger
	logger := engine.NewRequestLogger()
	if cfg.LogLevel != "" {
		logLevel, err := engine.ParseLogLevel(cfg.LogLevel)
		if err == nil {
			logger.SetLevel(logLevel)
		}
	}
	eng.SetLogger(logger)

	// Initialize runtime config state in the API layer
	api.InitRuntimeConfig(cfg.ProxyMode, cfg.ProxyAPIKeys, cfg.AdminPassword, cfg.DefaultTab, cfg.LogLevel)

	// Build admin API router
	adminRouter := api.NewRouter(s, eng, logger, iconFetcher, cfg, Version, BuildTime)
	webHandler := api.WebHandler()

	// Start listening
	errCh := make(chan error, 2)

	// TCP listener
	if cfg.BindAddr != "" {
		tcpListener, err := net.Listen("tcp", cfg.BindAddr)
		if err != nil {
			return fmt.Errorf("failed to bind TCP %s: %w", cfg.BindAddr, err)
		}
		defer tcpListener.Close()

		go func() {
			log.Printf("Tresor gateway listening on TCP %s", cfg.BindAddr)
			// The admin router serves both the admin API and the gateway handler
			// For now, we serve the combined router
			if err := engine.ServeProxy(eng, adminRouter.Handler(), webHandler, api.IsWebUIPath, tcpListener); err != nil {
				errCh <- fmt.Errorf("TCP serve error: %w", err)
			}
		}()
	}

	// Unix Domain Socket listener
	if cfg.SocketPath != "" {
		// Remove existing socket file
		os.Remove(cfg.SocketPath)

		udsListener, err := net.Listen("unix", cfg.SocketPath)
		if err != nil {
			return fmt.Errorf("failed to bind UDS %s: %w", cfg.SocketPath, err)
		}
		defer udsListener.Close()
		defer os.Remove(cfg.SocketPath)

		go func() {
			log.Printf("Tresor admin socket listening on %s", cfg.SocketPath)
			// UDS only serves the admin API (no proxy)
			if err := engine.ServeAdminOnly(adminRouter.UDSHandler(), udsListener); err != nil {
				errCh <- fmt.Errorf("UDS serve error: %w", err)
			}
		}()
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("Received signal %v, shutting down", sig)
	case err := <-errCh:
		log.Printf("Server error: %v", err)
		adminRouter.Stop()
		return err
	}

	adminRouter.Stop()
	return nil
}
