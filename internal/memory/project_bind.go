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

	// ErrBindPathClaimed means another project already records this
	// directory. Two projects on one checkout leave a session there resolving
	// to whichever row the prefix ranking happened to favour.
	ErrBindPathClaimed = errors.New("another project already records this path")

	// ErrBindPathContainsOther means another project's checkout is inside the
	// path being bound. A recorded path is a prefix match, so binding ~/git
	// while a project already records ~/git/infra would hand that project every
	// unregistered clone beneath it — the #546 shape, reached through the
	// command that is supposed to be the safe repair.
	ErrBindPathContainsOther = errors.New("the path contains another project's checkout")

	// ErrBindPathInsideOther means another project records a path this one is
	// inside, and that project records no repository remote. Resolution WOULD
	// return the nested project — the longest surviving path wins — so this is a
	// policy about overlapping claims, not a claim about a broken lookup. A
	// project identified only by its directory answers for every directory
	// beneath it, clones of unrelated repositories included, because nothing
	// contradicts it; nesting a second project inside that subtree entrenches
	// the overlap without giving the enclosing project the identity that would
	// make it safe. A remote is that identity, and a project with one is not in
	// this state: a session in a different repository then contradicts it, so a
	// nested checkout — a submodule, a vendored repository — is legitimately its
	// own project and binds.
	ErrBindPathInsideOther = errors.New("the path is inside another project's checkout")

	// ErrBindPathUnmatchable means the path is one path resolution cannot
	// match. Binding it would record a project no session directory can find,
	// which is the state the caller is trying to leave.
	ErrBindPathUnmatchable = errors.New("path resolution could never match this path")

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
// The recorded path is the PHYSICAL directory — path resolved through
// canonicalPath, the same EvalSymlinks resolution the session hook applies to a
// reported cwd. A path recorded as typed would be a spelling a session may
// never report, and would compare unequal to the physical path another project
// already records for the same checkout.
//
// Every refusal happens before the write, and a refusal means nothing changed.
// Re-binding the same project to the same path is not a refusal: it succeeds,
// reports Changed() == false, and leaves updated_at alone, so the command in
// the `ghost mcp status` hint is safe to re-run.
//
// path must be absolute and must satisfy StoredPathIsUsable; the caller checks
// that it exists and is a directory, because a caller that cannot look at the
// filesystem cannot know. detectedRemote is whatever the caller detected at
// that path, in any spelling — "" when the directory is not a checkout of
// anything.
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
	physical, err := canonicalPath(path)
	if err != nil {
		return result, fmt.Errorf("%w: %q cannot be resolved to a directory: %v", ErrBindPathUnusable, path, err)
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

	if err := checkBindPathConflicts(ctx, tx, id, physical); err != nil {
		return result, err
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

	// Compared as EXACT text against the physical path, and that strictness is
	// the point: the candidate query matches on stored text, so a row holding
	// "/x/checkout/" is returned by no clause of it — not equal to the session's
	// "/x/checkout", and not a prefix of it followed by "/" — and a project
	// stored that way resolves from nowhere. Treating two spellings of one
	// location as "already bound" would leave exactly that state unrepairable:
	// the refusal rolls back, the row keeps the spelling, and re-running fails
	// identically forever. It is also not in the unbound notice, whose shape
	// test passes, so nothing else would ever suggest the bind.
	//
	// The cost is that a row another writer spelled with backslashes is rewritten
	// once to the physical spelling, which the query does match. That is a
	// normalization, stable after the first bind.
	pathChanged := storedPath != physical
	newPath := storedPath
	if pathChanged {
		newPath = physical
	}
	remoteSet := remote != "" && storedRemote == ""
	newRemote := storedRemote
	if remoteSet {
		newRemote = remote
	}

	// The write happens before the resolvability check, not after: the check has
	// to ask the resolver's own query about the row as it will be once written
	// (its length filter, its prefix match and its longest-path rule all read
	// the stored value). Both are inside one transaction, so a refusal rolls the
	// write back and still leaves nothing written.
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

// checkBindPathConflicts refuses a path that would overlap another project's
// recorded location. Every comparison is between PHYSICAL paths, through the
// resolver's own canonicalPath and samePath, so a symlink to another project's
// checkout is recognised as the same place rather than a new one, and a
// containment decision is made on the same segment-boundary rule resolution
// uses to match a session directory.
//
// A recorded path that no longer resolves on disk is skipped: canonicalPath
// fails on it, and resolution would reject that candidate too (pathsAgree
// resolves both sides), so it can neither be the same directory nor shadow one.
func checkBindPathConflicts(ctx context.Context, tx *sql.Tx, id, physical string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, path, COALESCE(repo_remote, '') FROM projects WHERE id != ? AND id != '_global'`, id)
	if err != nil {
		return fmt.Errorf("read project paths: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var otherID, otherPath, otherRemote string
		if err := rows.Scan(&otherID, &otherPath, &otherRemote); err != nil {
			return fmt.Errorf("scan project path: %w", err)
		}
		if !storedPathIsUsable(otherPath) {
			continue
		}
		otherPhysical, err := canonicalPath(otherPath)
		if err != nil {
			continue
		}
		switch {
		case otherPhysical == physical:
			return fmt.Errorf("%w: %q already records %q — bind the path there, or merge the projects",
				ErrBindPathClaimed, otherID, otherPath)
		case samePath(otherPhysical, physical):
			// The other project is inside the path being bound, so this
			// project would claim every clone under it.
			return fmt.Errorf("%w: %q is inside it (%q) — binding the parent would claim every clone beneath it, which is how an unrelated directory reads another project's memories",
				ErrBindPathContainsOther, otherID, otherPath)
		case samePath(physical, otherPhysical) && otherRemote == "":
			// The path being bound is inside the other project's location, and
			// that project has no remote, so its directory claims the whole
			// subtree — a session in any clone under it resolves to it. The
			// remedy is the identity that ends the claim.
			return fmt.Errorf("%w: %q records %q and has no repository remote, so that directory answers for everything beneath it — bind the outer project instead, or give it a remote",
				ErrBindPathInsideOther, otherID, otherPath)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate project paths: %w", err)
	}
	return nil
}

// checkPathResolvable refuses a path that path resolution could not return for
// this project, and answers that by applying the resolver's own rules rather
// than a second set: the same candidate query, narrowed by the same
// agreesWithSession predicate, ranked by the same longest-path rule that
// ResolveProject applies before it returns a project.
//
// It must be called after the row is written inside the transaction, because
// every one of those rules reads the stored value — the candidate query drops
// recorded paths of ten characters or fewer, and the length that decides the
// longest match is the stored one. A refusal returns before Commit, so the
// rollback above undoes the write.
//
// It runs on the transaction rather than calling ResolveProject because that
// needs a second connection while this one holds the only one, and a second
// connection is a deadlock, not a race.
func checkPathResolvable(ctx context.Context, tx *sql.Tx, id, physical, remote string) error {
	hasRepoRemote, err := txHasRepoRemoteColumn(ctx, tx)
	if err != nil {
		return err
	}
	remoteColumn := "''"
	if hasRepoRemote {
		remoteColumn = "COALESCE(repo_remote, '')"
	}
	norm := absoluteSessionPath(physical)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(pathCandidatesQuery, remoteColumn), norm, norm, norm)
	if err != nil {
		return fmt.Errorf("read path candidates: %w", err)
	}
	candidates, err := scanBindCandidates(rows)
	if err != nil {
		return err
	}

	// The resolver keeps the single longest surviving candidate and errors on a
	// tie, so "would return this project" means exactly that: it is a survivor,
	// and no other survivor is at least as long.
	var bestID string
	bestLength := -1
	tied := false
	survivors := 0
	for _, candidate := range candidates {
		if !candidate.agreesWithSession(physical, remote) {
			continue
		}
		survivors++
		switch length := pathRankLength(candidate.path); {
		case length > bestLength:
			bestID, bestLength, tied = candidate.id, length, false
		case length == bestLength:
			tied = true
		}
	}
	if survivors == 0 {
		// Stated as what was observed, with the rule offered as the likely
		// reason rather than asserted as the cause: this project's own row
		// always survives agreesWithSession once it has been written with the
		// physical path, so the candidate query is what dropped it — and
		// blaming a rule the path may well pass is how the previous version of
		// this message came to deny the length limit to a 21-character path.
		//
		// pathRankLength, not len: the filter is SQLite's LENGTH() on TEXT,
		// which counts characters, so a byte count would report a multi-byte
		// path as longer than the limit that dropped it.
		return fmt.Errorf("%w: %q is %d characters — path resolution's candidate query returned no row for it, and that query only considers recorded paths longer than 10 characters",
			ErrBindPathUnmatchable, physical, pathRankLength(physical))
	}
	if tied {
		return fmt.Errorf("%w: %q ties with another project's path for the longest match, which resolution refuses to choose between",
			ErrBindPathUnmatchable, physical)
	}
	if bestID != id {
		return fmt.Errorf("%w: %q records a longer matching path, so resolution would return it instead",
			ErrBindPathUnmatchable, bestID)
	}
	return nil
}

// txHasRepoRemoteColumn is hasRepoRemoteColumn for a transaction, so the
// candidate query binds the same columns whether it runs on the pool or inside
// a bind.
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

// sameRecordedPath compared two recorded paths as locations rather than as text:
// separators normalized and dot segments cleaned on both sides. It is gone
// because resolution does not compare them that way. The candidate query matches
// the stored TEXT, so "/x/checkout/" and "/x/checkout" are not interchangeable
// there, and bind must write the spelling the query can return — see the
// pathChanged comparison in BindProjectPath.

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
