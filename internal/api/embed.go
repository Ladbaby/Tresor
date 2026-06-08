package api

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"tresor/internal/middleware"
)

//go:embed web/*
var webFS embed.FS

// forceRecompile ensures the embed is refreshed when web files change.
const forceRecompile_20260608_security = true

// WebHandler returns an http.Handler that serves the embedded web UI with
// security headers.
func WebHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		// Fallback: return a handler that always returns 404
		return http.NotFoundHandler()
	}
	return middleware.SecurityHeaders(http.FileServer(http.FS(sub)))
}

// IsWebUIPath checks whether the given path exists in the embedded web filesystem.
func IsWebUIPath(path string) bool {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		path = "."
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return false
	}
	_, err = sub.Open(path)
	return err == nil
}
