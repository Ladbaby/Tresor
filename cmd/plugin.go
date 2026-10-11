package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"tresor/internal/config"
)

const pluginUploadLimit = 8 << 20

func readPluginDirectory(dir string) (map[string]string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("plugin source must be a directory, not a symlink")
	}
	files := make(map[string]string)
	total := 0
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not allowed: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular plugin file: %s", path)
		}
		if len(files) >= 128 || info.Size() > pluginUploadLimit-int64(total) {
			return fmt.Errorf("plugin package exceeds upload limits")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, int64(pluginUploadLimit-total)+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		total += len(data)
		if total > pluginUploadLimit || !utf8.Valid(data) {
			return fmt.Errorf("plugin files must be bounded UTF-8 source")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := files["manifest.json"]; !ok {
		return nil, fmt.Errorf("manifest.json is required")
	}
	return files, nil
}

func pluginAPIRequest(cfg *config.AppConfig, method, path string, value any) ([]byte, error) {
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		if err != nil {
			return nil, err
		}
	}
	if len(body) > pluginUploadLimit {
		return nil, fmt.Errorf("encoded plugin request exceeds upload limit")
	}
	req, err := http.NewRequest(method, apiURL(cfg, path), bytes.NewReader(body))
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, pluginUploadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > pluginUploadLimit {
		return nil, fmt.Errorf("daemon response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func init() {
	group := &cobra.Command{Use: "plugin", Short: "Manage trusted local JavaScript packages"}
	group.AddCommand(&cobra.Command{Use: "install <directory>", Short: "Install a package version without activating it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		files, err := readPluginDirectory(args[0])
		if err != nil {
			return err
		}
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		data, err := pluginAPIRequest(cfg, http.MethodPost, "/api/plugin-packages/install", map[string]any{"files": files})
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}})
	group.AddCommand(&cobra.Command{Use: "activate <id> <version>", Short: "Activate an installed version for new requests", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		data, err := pluginAPIRequest(cfg, http.MethodPost, "/api/plugin-packages/activate", map[string]string{"id": args[0], "version": args[1]})
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}})
	group.AddCommand(&cobra.Command{Use: "list", Short: "List installed and bundled JavaScript packages", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		data, err := pluginAPIRequest(cfg, http.MethodGet, "/api/plugin-packages", nil)
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}})
	addPluginAuthorCommands(group)
	rootCmd.AddCommand(group)
}
