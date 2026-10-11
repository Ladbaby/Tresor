package jsplugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Manager struct {
	mu         sync.RWMutex
	dir        string
	reserved   map[string]bool
	installed  map[string]map[string]*Package
	active     map[string]string
	approvals  map[string]Approval
	provenance map[string]Provenance
}

func NewManager(dir string, reservedIDs []string) (*Manager, error) {
	m := &Manager{dir: dir, reserved: map[string]bool{}, installed: map[string]map[string]*Package{}, active: map[string]string{}, approvals: map[string]Approval{}, provenance: map[string]Provenance{}}
	for _, id := range reservedIDs {
		m.reserved[id] = true
	}
	if err := os.MkdirAll(filepath.Join(dir, "packages"), 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "packages"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, fmt.Errorf("unsafe cache entry %s", entry.Name())
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > MaxBundleBytes {
			return nil, fmt.Errorf("oversized cache entry")
		}
		data, err := os.ReadFile(filepath.Join(dir, "packages", entry.Name()))
		if err != nil {
			return nil, err
		}
		var files map[string]string
		if err := StrictJSON(data, &files); err != nil {
			return nil, err
		}
		pkg, err := loadPackage(files, true)
		if err != nil {
			return nil, err
		}
		if m.reserved[pkg.Manifest.ID] || entry.Name() != pkg.Digest+".json" {
			return nil, fmt.Errorf("cache identity mismatch")
		}
		if m.installed[pkg.Manifest.ID] == nil {
			m.installed[pkg.Manifest.ID] = map[string]*Package{}
		}
		if _, exists := m.installed[pkg.Manifest.ID][pkg.Manifest.Version]; exists {
			return nil, fmt.Errorf("duplicate cache identity")
		}
		m.installed[pkg.Manifest.ID][pkg.Manifest.Version] = pkg
	}
	if err := readMetadata(filepath.Join(dir, "active.json"), &m.active); err != nil {
		return nil, err
	}
	if m.active == nil {
		return nil, fmt.Errorf("active metadata must be an object")
	}
	for id, version := range m.active {
		if m.installed[id][version] == nil {
			return nil, fmt.Errorf("active package missing: %s@%s", id, version)
		}
	}
	if err := m.restoreMetadata(); err != nil {
		return nil, fmt.Errorf("plugin metadata: %w", err)
	}
	return m, nil
}
func atomicFile(file string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(file), ".staging-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
func (m *Manager) Install(files map[string]string) (Manifest, error) {
	return m.install(files, Provenance{Source: "local"})
}
func (m *Manager) install(files map[string]string, provenance Provenance) (Manifest, error) {
	pkg, err := LoadPackage(files)
	if err != nil {
		return Manifest{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reserved[pkg.Manifest.ID] {
		return Manifest{}, fmt.Errorf("reserved plugin ID")
	}
	if existing := m.installed[pkg.Manifest.ID][pkg.Manifest.Version]; existing != nil {
		if existing.Digest != pkg.Digest {
			return Manifest{}, fmt.Errorf("installed ID/version is immutable")
		}
		return existing.Manifest, nil
	}
	data, err := json.Marshal(pkg.files)
	if err != nil {
		return Manifest{}, err
	}
	packagePath := filepath.Join(m.dir, "packages", pkg.Digest+".json")
	next := make(map[string]Provenance, len(m.provenance)+1)
	for k, v := range m.provenance {
		next[k] = v
	}
	next[pkg.Digest] = provenance
	metadata, err := json.Marshal(next)
	if err == nil {
		err = atomicFile(filepath.Join(m.dir, "provenance.json"), metadata)
	}
	// Persist provenance before source: a crash must never turn a verified
	// remote install into an apparently local package. An orphan provenance
	// record is inert (no installed source or executable snapshot references it).
	if err != nil {
		return Manifest{}, err
	}
	if err := atomicFile(packagePath, data); err != nil {
		previous, marshalErr := json.Marshal(m.provenance)
		if marshalErr == nil {
			marshalErr = atomicFile(filepath.Join(m.dir, "provenance.json"), previous)
		}
		if marshalErr != nil {
			return Manifest{}, fmt.Errorf("package persistence failed; inert provenance retained: %w", err)
		}
		return Manifest{}, err
	}
	m.provenance = next
	if m.installed[pkg.Manifest.ID] == nil {
		m.installed[pkg.Manifest.ID] = map[string]*Package{}
	}
	m.installed[pkg.Manifest.ID][pkg.Manifest.Version] = pkg
	return pkg.Manifest, nil
}
func (m *Manager) Activate(id, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.installed[id][version] == nil {
		return fmt.Errorf("package version not installed")
	}
	if !m.installed[id][version].Compatible {
		return fmt.Errorf("incompatible API version: explicitly install and activate an API V2 package")
	}
	if !m.approved(m.installed[id][version]) {
		return fmt.Errorf("explicit digest-bound permission approval required")
	}
	next := make(map[string]string, len(m.active)+1)
	for id, v := range m.active {
		next[id] = v
	}
	next[id] = version
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := atomicFile(filepath.Join(m.dir, "active.json"), data); err != nil {
		return err
	}
	m.active = next
	return nil
}
func (m *Manager) Snapshot() map[string]*Package {
	packages, _ := m.ExecutionSnapshot()
	return packages
}
func (m *Manager) List() []PackageInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []PackageInfo{}
	for id, versions := range m.installed {
		for version, pkg := range versions {
			diagnostic := ""
			if !pkg.Compatible {
				diagnostic = "Unsupported API version; explicitly install and activate an API V2 package."
			}
			if pkg.Compatible && !m.approved(pkg) {
				diagnostic = "Explicit digest-bound permission approval required."
			}
			approved := append([]string{}, m.approvals[pkg.Digest].Permissions...)
			provenance, ok := m.provenance[pkg.Digest]
			if !ok {
				provenance = Provenance{Source: "local"}
			}
			out = append(out, PackageInfo{Manifest: pkg.Manifest, Digest: pkg.Digest, Active: m.active[id] == version && pkg.Compatible && m.approved(pkg), Selected: m.active[id] == version, Compatible: pkg.Compatible, Diagnostic: diagnostic, Executable: pkg.Compatible && m.approved(pkg), Approved: approved, Provenance: provenance})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Version < out[j].Version
		}
		return out[i].ID < out[j].ID
	})
	return out
}
