package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"tresor/internal/config"

	"github.com/spf13/cobra"
)

var ruleCmd = &cobra.Command{
	Use:   "rule",
	Short: "Manage routing rules",
}

func newHTTPClient(cfg *config.AppConfig) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.SocketPath != "" {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", cfg.SocketPath, 5*time.Second)
		}
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func apiURL(cfg *config.AppConfig, path string) string {
	if cfg.SocketPath != "" {
		return "http://localhost" + path
	}
	return "http://" + cfg.BindAddr + path
}

func doAPIRequest(cfg *config.AppConfig, method, path string, body []byte) ([]byte, error) {
	client := newHTTPClient(cfg)

	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequest(method, apiURL(cfg, path), bytes.NewReader(body))
	} else {
		req, err = http.NewRequest(method, apiURL(cfg, path), nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.AdminPassword != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AdminPassword)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

var ruleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all routing rules",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		body, err := doAPIRequest(cfg, http.MethodGet, "/api/rules", nil)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

var ruleSwitchCmd = &cobra.Command{
	Use:   "switch [rule-id] [downstream-id]",
	Short: "Switch a rule to a different downstream",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		payload := map[string]string{"downstream_id": args[1]}
		data, _ := json.Marshal(payload)

		body, err := doAPIRequest(cfg, http.MethodPut,
			fmt.Sprintf("/api/rules/%s/switch", args[0]), data)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

var ruleCreateCmd = &cobra.Command{
	Use:   "create [name] [pattern_path] [downstream_id]",
	Short: "Create a new routing rule",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"name":              args[0],
			"pattern_path":      args[1],
			"active_downstream": args[2],
			"pipeline_config":   "[]",
			"is_enabled":        true,
		}
		data, _ := json.Marshal(payload)

		body, err := doAPIRequest(cfg, http.MethodPost, "/api/rules", data)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

// ---- Alias CLI Commands ----

var aliasCmd = &cobra.Command{
	Use:   "alias",
	Short: "Manage model aliases (input-model -> output-model mapping)",
}

var aliasListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all alias groups with active mappings",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		body, err := doAPIRequest(cfg, http.MethodGet, "/api/aliases", nil)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

var aliasActivateCmd = &cobra.Command{
	Use:   "activate [alias-id]",
	Short: "Hot-switch: activate a specific alias option",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		body, err := doAPIRequest(cfg, http.MethodPut,
			fmt.Sprintf("/api/aliases/%s/activate", args[0]), nil)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

var aliasCreateCmd = &cobra.Command{
	Use:   "create [input-model-id] [downstream-id] [output-model-id]",
	Short: "Create a new alias option",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		payload := map[string]interface{}{
			"input_model_id":  args[0],
			"downstream_id":   args[1],
			"output_model_id": args[2],
			"is_active":       false,
		}
		data, _ := json.Marshal(payload)

		body, err := doAPIRequest(cfg, http.MethodPost, "/api/aliases", data)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

var aliasDeleteCmd = &cobra.Command{
	Use:   "delete [alias-id]",
	Short: "Delete an alias option",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		body, err := doAPIRequest(cfg, http.MethodDelete,
			fmt.Sprintf("/api/aliases/%s", args[0]), nil)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(ruleCmd)
	ruleCmd.AddCommand(ruleListCmd)
	ruleCmd.AddCommand(ruleSwitchCmd)
	ruleCmd.AddCommand(ruleCreateCmd)
	rootCmd.AddCommand(aliasCmd)
	aliasCmd.AddCommand(aliasListCmd)
	aliasCmd.AddCommand(aliasActivateCmd)
	aliasCmd.AddCommand(aliasCreateCmd)
	aliasCmd.AddCommand(aliasDeleteCmd)
}
