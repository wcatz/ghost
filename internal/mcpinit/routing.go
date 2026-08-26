package mcpinit

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// resolveSessionProject resolves a session cwd to a project, falling back to
// config routing.default_project for home-dir and filesystem-root sessions
// (issue #391). Everything else — direct hits, other unmatched dirs, an unset
// or unresolvable default — behaves exactly as a plain ResolveProject call.
func resolveSessionProject(ctx context.Context, store *memory.Store, cwd string) (id, name string) {
	id, name = resolve(ctx, store, cwd)
	if id != "" {
		return id, name
	}
	def := defaultProjectForSessions()
	if def == "" || !isHomeOrRoot(cwd) {
		return "", ""
	}
	return resolve(ctx, store, def) // "" on miss: degrade to today's no-match behavior
}

func resolve(ctx context.Context, store *memory.Store, input string) (string, string) {
	id, name, err := store.ResolveProject(ctx, input)
	if err != nil {
		return "", ""
	}
	return id, name
}

func defaultProjectForSessions() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	return cfg.Routing.DefaultProject
}

// isHomeOrRoot reports whether cwd is the user's home directory or the
// filesystem root — the two cwds that mean "not really working in a project".
func isHomeOrRoot(cwd string) bool {
	if cwd == "/" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if cwd == home {
		return true
	}
	// The session cwd may reach home through a symlink or trailing form;
	// compare resolved paths too.
	evalCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	evalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return false
	}
	return evalCwd == evalHome
}
