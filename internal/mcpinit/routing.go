package mcpinit

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

func resolveSessionProject(ctx context.Context, store *memory.Store, cwd string) (id, name string) {
	id, name = resolve(ctx, store, cwd)
	if id != "" {
		return id, name
	}
	def := defaultProjectForSessions()
	if def == "" || !isHomeOrRoot(cwd) {
		return "", ""
	}
	return resolve(ctx, store, def)
}

func resolve(ctx context.Context, store *memory.Store, input string) (string, string) {
	id, name, err := store.ResolveProject(ctx, input)
	if err != nil {
		return "", ""
	}
	return id, name
}

func defaultProjectForSessions() string {
	return config.LoadForHook().Routing.DefaultProject
}

func isHomeOrRoot(cwd string) bool {
	if isFilesystemRoot(cwd) {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	if cwd == home {
		return true
	}
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

func isFilesystemRoot(cwd string) bool {
	root := filepath.VolumeName(cwd) + string(os.PathSeparator)
	return filepath.Clean(cwd) == filepath.Clean(root)
}
