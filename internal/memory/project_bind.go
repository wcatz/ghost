package memory

// Binding a project to a checkout. Ghost identifies a project by the directory
// a session is standing in, falling back to a repository remote; a project
// that records neither can never be resolved from a directory and silently
// loses session-start injection and Stop-hook lifecycle work. That is the state
// every project upgraded from a v9 database is in — it stored its bare name as
// its path (the id sentinel) and had no repository column to hold a remote.
//
// #546 removed name-based resolution precisely so an unrelated clone could not
// claim a project, so the repair cannot be "resolve by name again". It is an
// explicit, single-purpose write: the user states which checkout is which
// project, and every claim it would overwrite is refused instead.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
)

// Reasons a bind can be refused. They are sentinels rather than formatted
// errors so a caller can tell "you asked for something impossible" from "the
// store could not do it", and so the CLI can print the command that fixes it.
var (
	// ErrBindProjectNotFound means no projects row carries that id. Bind takes
	// an id, not a name or a path: every other spelling can answer with a
	// different project than the one the user meant.
	ErrBindProjectNotFound = errors.New("no such project")

	// ErrBindGlobalProject means the target was _global. It is the bucket
	// every project's injection reads from, not a checkout, and giving it a
	// path would let a session standing anywhere claim global memories.
	ErrBindGlobalProject = errors.New("refusing to bind the _global project")

	// ErrBindPathUnusable means the path is not a location a session
	// directory can be compared against: relative, the bare root, or the id
	// sentinel. Storing one would produce a project that still never resolves,
	// which is the state the caller is trying to leave.
	ErrBindPathUnusable = errors.New("path is not a usable location")

	// ErrBindPathClaimed means another project already records this checkout.
	// Two projects on one directory leave a session there resolving to
	// whichever row the prefix ranking happened to favour.
	ErrBindPathClaimed = errors.New("another project already records this path")

	// ErrBindRemoteClaimed means another project already records the detected
	// repository. One repository is one project; binding a second would fork
	// its memories into a project nothing else can find.
	ErrBindRemoteClaimed = errors.New("another project already records this repository")

	// ErrBindRemoteConflict means the project is already identified as a
	// checkout of a different repository.
	ErrBindRemoteConflict = errors.New("the project already belongs to a different repository")
)

// ProjectBinding reports what one bind changed, so a caller can tell the user
// what moved and can stay quiet about the parts that did not.
type ProjectBinding struct {
	// ProjectID is the project that was bound.
	ProjectID string
	// Name is that project's display name, carried here so a caller printing
	// a report does not have to look the project up again.
	Name string
	// PreviousPath is the path recorded before the bind. It equals Path when
	// the bind was a no-op, and is what a report prints as the "from" side.
	PreviousPath string
	// Path is the location now recorded.
	Path string
	// RepoRemote is the normalized repository now recorded, "" if none.
	RepoRemote string
	// PathChanged and RemoteSet say which half of the bind took effect, so a
	// caller can distinguish a real repair from a re-run of the same command.
	PathChanged bool
	RemoteSet   bool
}

// Changed reports whether the bind wrote anything.
func (b ProjectBinding) Changed() bool { return b.PathChanged || b.RemoteSet }

// UnboundProject is a project no session directory can resolve: its recorded
// path is not a usable location and it records no repository. Such a project
// gets no session-start injection and no Stop-hook lifecycle work, silently.
type UnboundProject struct {
	ID   string
	Name string
	Path string
}

// StoredPathIsUsable reports whether stored is a location a session directory
// can be compared against — the rule resolution applies to a recorded path
// before it will agree with one. Exported for callers that validate a path
// before writing it, so "usable" has one definition rather than a second one
// next to the write.
func StoredPathIsUsable(stored string) bool { return storedPathIsUsable(stored) }

// BindProjectPath records path as the checkout for project id, and records
// detectedRemote as its repository when the project records none. Both halves
// happen in one transaction, so a refused remote never leaves a half-applied
// path behind.
//
// Every refusal happens before the write, and a refusal means nothing changed.
// Re-binding the same project to the same path is not a refusal: it succeeds,
// reports Changed() == false, and leaves updated_at alone, so the command in
// the `ghost mcp status` hint is safe to re-run.
//
// path must already be absolute and cleaned, and must satisfy
// StoredPathIsUsable; the caller resolves it against the filesystem because
// this package is a pure storage layer that never stats or spawns.
// detectedRemote is whatever the caller detected at that path, in any spelling
// — "" when the directory is not a checkout of anything.
func (s *Store) BindProjectPath(ctx context.Context, id, path, detectedRemote string) (ProjectBinding, error) {
	var result ProjectBinding

	if id == "_global" {
		return result, fmt.Errorf("%w: it holds every project's memories, not a checkout", ErrBindGlobalProject)
	}
	if id == "" {
		return result, fmt.Errorf("%w: empty project id", ErrBindProjectNotFound)
	}
	// Checked before the lookup below so a caller that passes a relative path
	// gets the reason it can never resolve, rather than "no such project".
	if !storedPathIsUsable(path) {
		return result, fmt.Errorf("%w: %q", ErrBindPathUnusable, path)
	}
	remote := NormalizeRepoRemote(detectedRemote)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin bind project tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var name, storedPath, storedRemote string
	err = tx.QueryRowContext(ctx,
		`SELECT name, path, COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).
		Scan(&name, &storedPath, &storedRemote)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("%w: %q", ErrBindProjectNotFound, id)
	}
	if err != nil {
		return result, fmt.Errorf("read project for binding: %w", err)
	}

	if owner, err := bindPathOwner(ctx, tx, id, path); err != nil {
		return result, err
	} else if owner != "" {
		return result, fmt.Errorf("%w: %q already records %q — bind the path there, or merge the projects", ErrBindPathClaimed, owner, path)
	}

	if remote != "" {
		if storedRemote != "" && storedRemote != remote {
			return result, fmt.Errorf("%w: %q is bound to %q, not %q — merge the projects instead of rebinding", ErrBindRemoteConflict, id, storedRemote, remote)
		}
		if storedRemote == "" {
			var owner string
			ownerErr := tx.QueryRowContext(ctx,
				`SELECT id FROM projects WHERE repo_remote = ? AND id NOT IN ('_global', ?) LIMIT 1`,
				remote, id).Scan(&owner)
			if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
				return result, fmt.Errorf("find repository owner: %w", ownerErr)
			}
			if owner != "" {
				return result, fmt.Errorf("%w: %q belongs to project %q — bind the path there, or merge the projects", ErrBindRemoteClaimed, remote, owner)
			}
		}
	}

	// A path that already reads as this location is left as it is, spelling
	// included: rewriting "/x/" to "/x" would report a change that changes
	// nothing, and the re-run of a bind must look like the re-run of a bind.
	pathChanged := !sameRecordedPath(storedPath, path)
	newPath := storedPath
	if pathChanged {
		newPath = path
	}
	remoteSet := remote != "" && storedRemote == ""
	newRemote := storedRemote
	if remoteSet {
		newRemote = remote
	}

	if pathChanged || remoteSet {
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET path = ?, repo_remote = ?, updated_at = datetime('now') WHERE id = ?`,
			newPath, newRemote, id); err != nil {
			return result, fmt.Errorf("write project binding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit bind project tx: %w", err)
	}

	return ProjectBinding{
		ProjectID:    id,
		Name:         name,
		PreviousPath: storedPath,
		Path:         newPath,
		RepoRemote:   newRemote,
		PathChanged:  pathChanged,
		RemoteSet:    remoteSet,
	}, nil
}

// bindPathOwner returns the id of another project that already records path,
// or "" when the path is free. The whole table is scanned rather than matched
// in SQL because equality has to hold after cleaning and separator
// normalization on both sides, which SQLite cannot do the same way resolution
// does; a projects table holds a handful of rows.
func bindPathOwner(ctx context.Context, tx *sql.Tx, id, path string) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, path FROM projects WHERE id != ? AND id != '_global'`, id)
	if err != nil {
		return "", fmt.Errorf("read project paths: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var otherID, otherPath string
		if err := rows.Scan(&otherID, &otherPath); err != nil {
			return "", fmt.Errorf("scan project path: %w", err)
		}
		if sameRecordedPath(otherPath, path) {
			return otherID, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate project paths: %w", err)
	}
	return "", nil
}

// sameRecordedPath compares two recorded paths as locations rather than as
// text: separators normalized and dot segments cleaned on both sides, so
// "/x/checkout/" and "\x\checkout" are one directory. Deliberately not
// filepath.Clean, which would resolve a relative path against this process's
// working directory and make the answer depend on where the caller runs.
func sameRecordedPath(a, b string) bool {
	return cleanRecordedPath(a) == cleanRecordedPath(b)
}

func cleanRecordedPath(p string) string {
	return pathpkg.Clean(strings.ReplaceAll(p, `\`, "/"))
}

// ListUnboundProjects returns the projects a session directory can never
// resolve: no usable recorded path and no repository remote, in name order.
// _global is excluded — it is a shared bucket rather than a checkout, and it
// is never resolved from a directory in the first place.
func (s *Store) ListUnboundProjects(ctx context.Context) ([]UnboundProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hasRepoRemote, err := s.hasRepoRemoteColumn(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect project schema: %w", err)
	}
	remoteColumn := "''"
	if hasRepoRemote {
		remoteColumn = "COALESCE(repo_remote, '')"
	}

	// The filter is in Go because storedPathIsUsable is a Go predicate, and
	// duplicating its three refusals in SQL would give the notice and
	// resolution two definitions of the same rule.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, path, `+remoteColumn+` FROM projects WHERE id != '_global' ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var unbound []UnboundProject
	for rows.Next() {
		var p struct {
			id, name, path, remote string
		}
		if err := rows.Scan(&p.id, &p.name, &p.path, &p.remote); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		if p.remote != "" || storedPathIsUsable(p.path) {
			continue
		}
		unbound = append(unbound, UnboundProject{ID: p.id, Name: p.name, Path: p.path})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return unbound, nil
}
