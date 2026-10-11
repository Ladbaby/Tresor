package jsplugins

import (
	"encoding/json"
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

func (m *Manager) approved(pkg *Package) bool {
	grant := m.approvals[pkg.Digest]
	for _, permission := range pkg.Manifest.Permissions {
		found := false
		for _, p := range grant.Permissions {
			if permission == p {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (m *Manager) permissionChange(id, version, digest string, permissions []string, revoke bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg := m.installed[id][version]
	if pkg == nil || pkg.Digest != digest {
		return fmt.Errorf("installed package digest does not match")
	}
	if !pkg.Compatible {
		return fmt.Errorf("unsupported API version")
	}
	if len(permissions) == 0 {
		return fmt.Errorf("explicit permissions required")
	}
	seen := map[string]bool{}
	for _, p := range permissions {
		if p != "log.v1" || seen[p] {
			return fmt.Errorf("unknown/duplicate permission")
		}
		seen[p] = true
		declared := false
		for _, q := range pkg.Manifest.Permissions {
			if q == p {
				declared = true
			}
		}
		if !declared {
			return fmt.Errorf("permission not declared")
		}
	}
	next := make(map[string]Approval, len(m.approvals)+1)
	for k, v := range m.approvals {
		next[k] = v
	}
	granted := map[string]bool{}
	for _, p := range next[digest].Permissions {
		granted[p] = true
	}
	for _, p := range permissions {
		if revoke {
			delete(granted, p)
		} else {
			granted[p] = true
		}
	}
	list := []string{}
	if granted["log.v1"] {
		list = append(list, "log.v1")
	}
	if len(list) == 0 {
		delete(next, digest)
	} else {
		next[digest] = Approval{id, version, list}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := atomicFile(filepath.Join(m.dir, "approvals.json"), data); err != nil {
		return err
	}
	m.approvals = next
	return nil
}
func (m *Manager) Approve(id, version, digest string, permissions []string) error {
	return m.permissionChange(id, version, digest, permissions, false)
}
func (m *Manager) Revoke(id, version, digest string, permissions []string) error {
	return m.permissionChange(id, version, digest, permissions, true)
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
	if err := readMetadata(filepath.Join(m.dir, "approvals.json"), &m.approvals); err != nil {
		return err
	}
	for digest, a := range m.approvals {
		pkg := m.installed[a.ID][a.Version]
		if pkg == nil || pkg.Digest != digest || len(a.Permissions) != 1 || a.Permissions[0] != "log.v1" {
			return fmt.Errorf("invalid approval metadata")
		}
		declared := false
		for _, p := range pkg.Manifest.Permissions {
			if p == "log.v1" {
				declared = true
			}
		}
		if !declared {
			return fmt.Errorf("approval not declared")
		}
	}
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
	if m.approvals == nil || m.provenance == nil {
		return fmt.Errorf("metadata must be objects")
	}
	return nil
}

// ExecutionSnapshot publishes availability and effective grants under one lock.
// Package copies pin grants without ever mutating installed source snapshots.
func (m *Manager) ExecutionSnapshot() (map[string]*Package, map[string]string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	packages := map[string]*Package{}
	blocked := map[string]string{}
	for id, version := range m.active {
		pkg := m.installed[id][version]
		if !pkg.Compatible {
			blocked[id] = "requires explicit API V2 installation and activation"
			continue
		}
		if !m.approved(pkg) {
			blocked[id] = "requires explicit digest-bound permission approval"
			continue
		}
		copy := *pkg
		copy.granted = map[string]bool{}
		for _, p := range m.approvals[pkg.Digest].Permissions {
			copy.granted[p] = true
		}
		packages[id] = &copy
	}
	return packages, blocked
}
