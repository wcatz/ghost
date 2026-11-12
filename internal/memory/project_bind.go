package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	ErrBindProjectNotFound   = errors.New("no such project")
	ErrBindGlobalProject     = errors.New("refusing to bind the _global project")
	ErrBindPathUnusable      = errors.New("path is not a usable location")
	ErrBindPathUnprintable   = errors.New("path holds something Ghost cannot store")
	ErrBindPathClaimed       = errors.New("another project already records this path")
	ErrBindPathContainsOther = errors.New("the path contains another project's checkout")
	ErrBindPathInsideOther   = errors.New("the path is inside another project's checkout")
	ErrBindPathUnmatchable   = errors.New("path resolution could never match this path")
	ErrBindRemoteClaimed     = errors.New("another project already records this repository")
	ErrBindRemoteConflict    = errors.New("the project already belongs to a different repository")
)

func projectRef(name, id string) string {
	rendered := RefusalArg(ProjectIdentifierLabel, id)
	if name == "" || name == id {
		return rendered
	}
	return fmt.Sprintf("%q (%s)", name, rendered)
}

// ProjectBinding reports what one bind changed, so a caller can tell the user
// what moved and can stay quiet about the parts that did not.
type ProjectBinding struct {
	ProjectID    string
	Name         string
	PreviousPath string
	Path         string
	RepoRemote   string
	PathChanged  bool
	RemoteSet    bool
}

func (b ProjectBinding) Changed() bool { return b.PathChanged || b.RemoteSet }

// UnboundProject is a project no session directory can resolve: its recorded
// path is not a usable location and it records no repository. Such a project
type UnboundProject struct {
	ID   string
	Name string
	Path string
}

// StoredPathIsUsable reports whether stored is a location a session directory
// can be compared against — the rule resolution applies to a recorded path
func StoredPathIsUsable(stored string) bool { return storedPathIsUsable(stored) }

func (s *Store) BindProjectPath(ctx context.Context, id, path, detectedRemote string) (ProjectBinding, error) {
	var result ProjectBinding

	if id == "_global" {
		return result, fmt.Errorf("%w: it holds every project's memories, not a checkout", ErrBindGlobalProject)
	}
	if id == "" {
		return result, fmt.Errorf("%w: empty project id", ErrBindProjectNotFound)
	}
	if !storedPathIsUsable(path) {
		return result, fmt.Errorf("%w: %q", ErrBindPathUnusable, path)
	}
	physical, err := canonicalPath(path)
	if err != nil {
		return result, fmt.Errorf("%w: %q cannot be resolved to a directory: %v", ErrBindPathUnusable, path, err)
	}

	if err := rejectSecret("path", physical); err != nil {
		return result, fmt.Errorf("%w: %w", ErrBindPathUnprintable, err)
	}
	if err := CheckImportedProjectText("path", physical); err != nil {
		return result, fmt.Errorf("%w: %w", ErrBindPathUnprintable, err)
	}
	remote := NormalizeRepoRemote(detectedRemote)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "bind-project")
	if err != nil {
		return result, fmt.Errorf("begin bind project tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var name, storedPath, storedRemote string
	err = tx.QueryRowContext(ctx,
		`SELECT name, path, COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).
		Scan(&name, &storedPath, &storedRemote)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("%w: %s", ErrBindProjectNotFound, RefusalArg(ProjectIdentifierLabel, id))
	}
	if err != nil {
		return result, fmt.Errorf("read project for binding: %w", err)
	}

	if err := checkBindPathConflicts(ctx, tx, id, physical); err != nil {
		return result, err
	}

	if remote != "" {
		if storedRemote != "" && storedRemote != remote {
			return result, fmt.Errorf("%w: %s is bound to %q, not %q — merge the projects instead of rebinding", ErrBindRemoteConflict, RefusalArg(ProjectIdentifierLabel, id), storedRemote, remote)
		}
		if storedRemote == "" {
			var owner, ownerName string
			ownerErr := tx.QueryRowContext(ctx,
				`SELECT id, name FROM projects WHERE repo_remote = ? AND id NOT IN ('_global', ?) LIMIT 1`,
				remote, id).Scan(&owner, &ownerName)
			if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
				return result, fmt.Errorf("find repository owner: %w", ownerErr)
			}
			if owner != "" {
				return result, fmt.Errorf("%w: %q belongs to project %s — bind the path there, or merge the projects", ErrBindRemoteClaimed, remote, projectRef(ownerName, owner))
			}
		}
	}

	pathChanged := storedPath != physical
	if pathChanged && storedPathIsUsable(storedPath) {
		if storedCanon, cErr := canonicalPath(storedPath); cErr == nil && storedCanon == physical {
			pathChanged = false
		}
	}
	newPath := storedPath
	if pathChanged {
		newPath = physical
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
	if err := checkPathResolvable(ctx, tx, id, physical, remote); err != nil {
		return result, err
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

func checkBindPathConflicts(ctx context.Context, tx *sql.Tx, id, physical string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, path, COALESCE(repo_remote, '') FROM projects WHERE id != ? AND id != '_global'`, id)
	if err != nil {
		return fmt.Errorf("read project paths: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var otherID, otherName, otherPath, otherRemote string
		if err := rows.Scan(&otherID, &otherName, &otherPath, &otherRemote); err != nil {
			return fmt.Errorf("scan project path: %w", err)
		}
		if !storedPathIsUsable(otherPath) {
			continue
		}
		otherPhysical, err := canonicalPath(otherPath)
		if err != nil {
			continue
		}
		other := projectRef(otherName, otherID)
		switch {
		case otherPhysical == physical:
			return fmt.Errorf("%w: %s already records %q — bind the path there, or merge the projects",
				ErrBindPathClaimed, other, otherPath)
		case samePath(otherPhysical, physical):
			return fmt.Errorf("%w: %s is inside it (%q) — binding the parent would claim every clone beneath it, which is how an unrelated directory reads another project's memories",
				ErrBindPathContainsOther, other, otherPath)
		case samePath(physical, otherPhysical) && otherRemote == "":
			return fmt.Errorf("%w: %s records %q and has no repository remote, so that directory answers for everything beneath it — bind the outer project instead, or give it a remote",
				ErrBindPathInsideOther, other, otherPath)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate project paths: %w", err)
	}
	return nil
}

func checkPathResolvable(ctx context.Context, tx *sql.Tx, id, physical, remote string) error {
	hasRepoRemote, err := txHasRepoRemoteColumn(ctx, tx)
	if err != nil {
		return err
	}
	remoteColumn := "''"
	if hasRepoRemote {
		remoteColumn = "COALESCE(repo_remote, '')"
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name, path, `+remoteColumn+` FROM projects`)
	if err != nil {
		return fmt.Errorf("read project paths: %w", err)
	}
	candidates, err := scanBindCandidates(rows)
	if err != nil {
		return err
	}

	inputCanon, err := canonicalPath(physical)
	if err != nil {
		return fmt.Errorf("%w: %q cannot be resolved: %v", ErrBindPathUnmatchable, physical, err)
	}
	var bestID string
	bestLength := -1
	tied := false
	survivors := 0
	for _, candidate := range candidates {
		if !storedPathIsUsable(candidate.path) || candidate.remoteConflicts(remote) {
			continue
		}
		canon, cErr := canonicalPath(candidate.path)
		if cErr != nil || !samePath(inputCanon, canon) {
			continue
		}
		survivors++
		switch length := pathRankLength(canon); {
		case length > bestLength:
			bestID, bestLength, tied = candidate.id, length, false
		case length == bestLength:
			tied = true
		}
	}
	if survivors == 0 {
		return fmt.Errorf("%w: %q matched no recorded path in resolved form, so no session directory would resolve to it",
			ErrBindPathUnmatchable, physical)
	}
	if tied {
		return fmt.Errorf("%w: %q ties with another project's path for the longest match, which resolution refuses to choose between",
			ErrBindPathUnmatchable, physical)
	}
	if bestID != id {
		return fmt.Errorf("%w: %s records a longer matching path, so resolution would return it instead",
			ErrBindPathUnmatchable, RefusalArg(ProjectIdentifierLabel, bestID))
	}
	return nil
}

func txHasRepoRemoteColumn(ctx context.Context, tx *sql.Tx) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM pragma_table_info('projects') WHERE name = 'repo_remote'
	`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect project schema: %w", err)
	}
	return exists > 0, nil
}

func scanBindCandidates(rows *sql.Rows) ([]basenameCandidate, error) {
	defer rows.Close() //nolint:errcheck
	var candidates []basenameCandidate
	for rows.Next() {
		var candidate basenameCandidate
		if err := rows.Scan(&candidate.id, &candidate.name, &candidate.path, &candidate.remote); err != nil {
			return nil, fmt.Errorf("scan path candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate path candidates: %w", err)
	}
	return candidates, nil
}

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
