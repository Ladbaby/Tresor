package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"tresor/internal/config"

	"github.com/spf13/cobra"
)

func aliasAPI(method, path string, patch any) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	var body []byte
	if patch != nil {
		body, err = json.Marshal(patch)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, apiURL(cfg, path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.AdminPassword != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AdminPassword)
	}
	resp, err := newHTTPClient(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("daemon HTTP %d: %s", resp.StatusCode, data)
	}
	fmt.Println(string(data))
	return nil
}

func init() {
	aliases := &cobra.Command{Use: "alias", Short: "Manage alias routing modes and priority"}
	aliases.AddCommand(&cobra.Command{Use: "list", Short: "List alias groups", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return aliasAPI(http.MethodGet, "/api/aliases", nil) }})
	aliases.AddCommand(&cobra.Command{Use: "mode <input-model-id> <mode>", Short: "Set default, load_balance, or fallback routing", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		switch args[1] {
		case "default", "load_balance", "fallback":
		default:
			return fmt.Errorf("invalid route mode %q", args[1])
		}
		return aliasAPI(http.MethodPut, "/api/aliases/group/"+url.PathEscape(args[0]), map[string]any{"route_mode": args[1]})
	}})
	aliases.AddCommand(&cobra.Command{Use: "reorder-options <input-model-id> <option-id>...", Short: "Set complete alias option priority order", Args: cobra.MinimumNArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		return aliasAPI(http.MethodPut, "/api/aliases/group/"+url.PathEscape(args[0]), map[string]any{"option_order": args[1:]})
	}})
	rootCmd.AddCommand(aliases)
}
