package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"tresor/internal/config"
	"tresor/internal/jsplugins"
)

func addPluginAuthorCommands(group *cobra.Command) {
	var strict, jsonOutput bool
	lint := &cobra.Command{Use: "lint <directory>", Short: "Statically lint author conventions without executing source", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		files, err := readPluginDirectory(args[0])
		if err != nil {
			return err
		}
		diagnostics, err := jsplugins.Lint(files)
		if err != nil {
			return err
		}
		if jsonOutput {
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(diagnostics); err != nil {
				return err
			}
		} else {
			for _, d := range diagnostics {
				cmd.Printf("%s:%d:%d %s %s: %s\n", d.File, d.Line, d.Column, d.Severity, d.Code, d.Message)
			}
			cmd.Printf("%d advisory findings (static analysis is not a compatibility proof)\n", len(diagnostics))
		}
		if strict && len(diagnostics) > 0 {
			return fmt.Errorf("strict author lint found warnings")
		}
		return nil
	}}
	lint.Flags().BoolVar(&strict, "strict", false, "Fail on advisory findings")
	lint.Flags().BoolVar(&jsonOutput, "json", false, "Print structured diagnostics")
	group.AddCommand(lint)
	var output string
	pack := &cobra.Command{Use: "pack <directory>", Short: "Export file-map bundle and canonical SHA-256 without execution", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if output == "" {
			return fmt.Errorf("--output is required")
		}
		files, err := readPluginDirectory(args[0])
		if err != nil {
			return err
		}
		data, digest, err := jsplugins.Pack(files)
		if err != nil {
			return err
		}
		// Do not silently overwrite another artifact.
		f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		cmd.Println(digest)
		return nil
	}}
	pack.Flags().StringVar(&output, "output", "", "New bundle output file")
	group.AddCommand(pack)
	var digest, signature, key string
	remote := &cobra.Command{Use: "install-url <https-url>", Short: "Install and enable a trusted HTTPS package", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		data, err := pluginAPIRequest(cfg, http.MethodPost, "/api/plugin-packages/install-remote", jsplugins.RemoteInput{URL: args[0], SHA256: digest, Signature: signature, PublicKey: key})
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}}
	remote.Flags().StringVar(&digest, "sha256", "", "Optional expected canonical package digest")
	remote.Flags().StringVar(&signature, "signature", "", "Base64 Ed25519 signature")
	remote.Flags().StringVar(&key, "public-key", "", "Base64 publisher public key supplied out of band")
	group.AddCommand(remote)
}
