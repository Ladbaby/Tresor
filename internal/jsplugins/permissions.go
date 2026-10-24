package jsplugins

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

type Approval struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Permissions []string `json:"permissions"`
}
type Provenance struct {
	Source    string `json:"source"`
	URL       string `json:"url,omitempty"`
	Verified  bool   `json:"verified"`
	Publisher string `json:"publisher,omitempty"`
	Signed    bool   `json:"signed"`
}

func readMetadata(file string, value any) error {
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxSourceBytes {
		return fmt.Errorf("unsafe metadata file")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return StrictJSON(data, value)
}
func (m *Manager) restoreMetadata() error {
	// Legacy grants are intentionally ignored: privileged logging no longer exists.
	if err := readMetadata(filepath.Join(m.dir, "provenance.json"), &m.provenance); err != nil {
		return err
	}
	for digest, p := range m.provenance {
		if !validDigest(digest) || (p.Source != "local" && p.Source != "remote") {
			return fmt.Errorf("invalid provenance identity")
		}
		if p.Source == "local" {
			if p.URL != "" || p.Verified || p.Signed || p.Publisher != "" {
				return fmt.Errorf("invalid local provenance")
			}
			continue
		}
		u, err := url.Parse(p.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(p.URL) > 4096 || p.Signed && (!p.Verified || !validDigest(p.Publisher)) || p.Signed != (p.Publisher != "") {
			return fmt.Errorf("invalid remote provenance")
		}
	}
	if m.provenance == nil {
		return fmt.Errorf("metadata must be objects")
	}
	return nil
}

// ExecutionSnapshot pins enabled immutable packages under one lock.
func (m *Manager) ExecutionSnapshot() (map[string]*Package, map[string]string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	packages := map[string]*Package{}
	blocked := map[string]string{}
	for id, version := range m.active {
		pkg := m.installed[id][version]
		if !pkg.Compatible {
			blocked[id] = "unsupported package capabilities"
			continue
		}
		packages[id] = pkg
	}
	return packages, blocked
}
