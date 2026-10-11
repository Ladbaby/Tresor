package jsplugins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// SourceDigest validates and identifies source without compiling or evaluating it.
func SourceDigest(files map[string]string) (string, error) {
	if len(files) == 0 || len(files) > MaxFiles {
		return "", fmt.Errorf("invalid package file count")
	}
	names := make([]string, 0, len(files))
	fold := map[string]bool{}
	total := 0
	for name, text := range files {
		total += len(name) + len(text)
		if !safeFile(name) || !utf8.ValidString(text) || fold[strings.ToLower(name)] {
			return "", fmt.Errorf("unsafe/duplicate file %q", name)
		}
		fold[strings.ToLower(name)] = true
		names = append(names, name)
	}
	if total > MaxSourceBytes {
		return "", fmt.Errorf("package exceeds source limit")
	}
	if _, ok := files["manifest.json"]; !ok {
		return "", fmt.Errorf("manifest.json is required")
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%d:%s%d:%s", len(name), name, len(files[name]), files[name])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StrictJSON rejects duplicate keys at every depth and trailing values.
func StrictJSON(data []byte, value any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("JSON must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return fmt.Errorf("duplicate/invalid JSON key")
				}
				seen[s] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func Pack(files map[string]string) ([]byte, string, error) {
	digest, err := SourceDigest(files)
	if err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(struct {
		Files map[string]string `json:"files"`
	}{files})
	return data, digest, err
}
