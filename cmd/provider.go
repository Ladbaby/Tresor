package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"tresor/internal/config"

	"github.com/spf13/cobra"
)

// providerAPI talks to the daemon's provider endpoints. Responses that carry a
// JSON body are printed verbatim so scripts can pipe them into jq.
func providerAPI(method, path string, body any) ([]byte, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(method, apiURL(cfg, path), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.AdminPassword != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AdminPassword)
	}
	resp, err := newHTTPClient(cfg).Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// readProfileArg accepts "-" for stdin or a path to a profile JSON file, so a
// user can install a profile from disk without hand-quoting JSON on a command
// line.
func readProfileArg(arg string) ([]byte, error) {
	if arg == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(arg)
}

func newProviderCmd() *cobra.Command {
	providers := &cobra.Command{
		Use:     "provider",
		Aliases: []string{"profile"},
		Short:   "Install and manage provider profiles",
		Long: "Provider profiles are declarative provider definitions: a base URL, API\n" +
			"formats, model list, and optionally the plugins and aliases the provider\n" +
			"needs. Installing one creates an ordinary provider in the Providers tab, so\n" +
			"`provider installed` lists it and it can be edited by hand afterwards.\n\n" +
			"A profile never contains credentials; you supply them at install time.",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List installable providers from the online catalog",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if refresh, _ := cmd.Flags().GetBool("refresh"); refresh {
				if _, err := providerAPI(http.MethodPost, "/api/provider-catalog/refresh", map[string]any{}); err != nil {
					return err
				}
			}
			data, err := providerAPI(http.MethodGet, "/api/provider-catalog", nil)
			if err != nil {
				return err
			}
			return printCatalog(data)
		},
	}
	list.Flags().Bool("refresh", false, "Fetch the remote index before listing")
	providers.AddCommand(list)

	providers.AddCommand(&cobra.Command{
		Use:   "installed",
		Short: "List installed providers with their profile version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			data, err := providerAPI(http.MethodGet, "/api/providers", nil)
			if err != nil {
				return err
			}
			return printInstalled(data)
		},
	})

	install := &cobra.Command{
		Use:   "install <profile-id | file | ->",
		Short: "Install a provider from the catalog, a file, or stdin",
		Long: "Install a provider profile. The argument is either a catalog id\n" +
			"(see `provider list`), a path to a profile JSON file, or \"-\" for stdin.\n\n" +
			"Supply the provider's API key with --api-key or TRESOR_PROVIDER_API_KEY.\n" +
			"Providers that need a client secret take --client-secret.\n\n" +
			"Use --allow-plugins to let the profile bind a plugin; profile plugin\n" +
			"bindings are skipped without it, because a plugin is code, not data.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var payload map[string]any
			if !looksLikePath(args[0]) {
				payload = map[string]any{"profile_id": args[0]}
			} else {
				raw, err := readProfileArg(args[0])
				if err != nil {
					return err
				}
				payload = map[string]any{"profile": json.RawMessage(raw)}
			}

			apiKey, _ := cmd.Flags().GetString("api-key")
			if apiKey == "" {
				apiKey = os.Getenv("TRESOR_PROVIDER_API_KEY")
			}
			if apiKey != "" {
				payload["api_key"] = apiKey
			}
			if secret, _ := cmd.Flags().GetString("client-secret"); secret != "" {
				payload["client_secret"] = secret
			}
			if allow, _ := cmd.Flags().GetBool("allow-plugins"); allow {
				payload["allow_plugins"] = true
			}
			if id, _ := cmd.Flags().GetString("downstream-id"); id != "" {
				payload["downstream_id"] = id
			}
			if region, _ := cmd.Flags().GetString("region"); region != "" {
				payload["region"] = region
			}

			_, err := providerAPI(http.MethodPost, "/api/providers/install", payload)
			if err != nil {
				return err
			}
			fmt.Println("installed")
			return nil
		},
	}
	install.Flags().String("api-key", "", "Provider API key (or set TRESOR_PROVIDER_API_KEY)")
	install.Flags().String("client-secret", "", "OAuth client secret, when the provider needs one")
	install.Flags().Bool("allow-plugins", false, "Allow the profile to bind plugins")
	install.Flags().String("downstream-id", "", "Use a specific provider id instead of deriving one")
	install.Flags().String("region", "", "Install at one of the profile's regions (see `provider list`)")
	providers.AddCommand(install)

	providers.AddCommand(&cobra.Command{
		Use:   "update <provider-id> <profile-id | file | ->",
		Short: "Apply a newer profile version without discarding local edits",
		Long: "Update an installed provider to a newer profile.\n\n" +
			"Fields you have not changed are updated; fields you edited by hand are\n" +
			"kept and reported, so an update never silently reverts your configuration.\n" +
			"Credentials and the on/off state are always preserved.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			var payload map[string]any
			if !looksLikePath(args[1]) {
				payload = map[string]any{"profile_id": args[1]}
			} else {
				raw, err := readProfileArg(args[1])
				if err != nil {
					return err
				}
				payload = map[string]any{"profile": json.RawMessage(raw)}
			}
			data, err := providerAPI(http.MethodPost, "/api/providers/"+url.PathEscape(args[0])+"/update", payload)
			if err != nil {
				return err
			}
			return printUpdate(data)
		},
	})

	providers.AddCommand(&cobra.Command{
		Use:   "remove <provider-id>",
		Short: "Remove an installed provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if _, err := providerAPI(http.MethodDelete, "/api/providers/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			fmt.Println("removed")
			return nil
		},
	})

	return providers
}

// looksLikePath reports whether the argument names a file or stdin rather than a
// catalog id. Catalog ids are simple slugs, so a path separator or an existing
// file is decisive.
func looksLikePath(arg string) bool {
	if arg == "-" {
		return true
	}
	if strings.ContainsAny(arg, `/\.`) {
		return true
	}
	_, err := os.Stat(arg)
	return err == nil
}

// printCatalog renders the catalog as an aligned list, marking entries this
// build cannot run and providers that are already installed.
func printCatalog(data []byte) error {
	var body struct {
		Entries []struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			Version   int      `json:"version"`
			Available bool     `json:"available"`
			Reason    string   `json:"reason"`
			Installed []string `json:"installed"`
			Regions   []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"regions"`
		} `json:"entries"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return err
	}
	if len(body.Entries) == 0 {
		fmt.Println("No providers in the catalog.")
		return nil
	}
	for _, e := range body.Entries {
		status := "installable"
		switch {
		case len(e.Installed) > 0:
			status = "installed as " + strings.Join(e.Installed, ", ")
		case !e.Available:
			status = "unavailable"
		}
		fmt.Printf("  %-20s %-28s v%-4d %s\n", e.ID, e.Name, e.Version, status)
		if !e.Available && e.Reason != "" {
			fmt.Printf("  %-20s %s\n", "", e.Reason)
		}
		// Regions are listed under the entry so a user can pick one with
		// `provider install --region`, instead of reading the recipe.
		if len(e.Regions) > 0 {
			labels := make([]string, 0, len(e.Regions))
			for _, r := range e.Regions {
				label := r.ID
				if r.Name != "" && r.Name != r.ID {
					label = r.ID + " (" + r.Name + ")"
				}
				labels = append(labels, label)
			}
			fmt.Printf("  %-20s regions: %s\n", "", strings.Join(labels, ", "))
		}
	}
	fmt.Printf("\nsource: %s\n", body.Source)
	return nil
}

func printInstalled(data []byte) error {
	var items []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		Profile *struct {
			ProfileID string `json:"profile_id"`
			Version   int    `json:"version"`
			Source    string `json:"source"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("No providers installed.")
		return nil
	}
	for _, it := range items {
		if it.Profile == nil {
			continue
		}
		fmt.Printf("  %-20s %-24s %s v%d (%s)\n", it.ID, it.Name, it.Profile.ProfileID, it.Profile.Version, it.Profile.Source)
	}
	return nil
}

func printUpdate(data []byte) error {
	var res struct {
		Applied   []string `json:"applied"`
		Kept      []string `json:"kept"`
		Conflicts []string `json:"conflicts"`
		Version   int      `json:"version"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return err
	}
	fmt.Printf("updated to profile v%d\n", res.Version)
	if len(res.Applied) > 0 {
		fmt.Printf("  updated: %s\n", strings.Join(res.Applied, ", "))
	}
	if len(res.Kept) > 0 {
		fmt.Printf("  kept your edits: %s\n", strings.Join(res.Kept, ", "))
	}
	for _, c := range res.Conflicts {
		fmt.Printf("  note: %s\n", c)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newProviderCmd())
}
