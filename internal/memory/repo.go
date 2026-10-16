package memory

import "strings"

// NormalizeRepoRemote reduces a repository remote to a canonical form so the
// spellings of one repository compare equal.
//
// The same remote is written differently by every tool that touches it: git
// prints scp-style `git@github.com:owner/repo.git` for SSH, agents and humans
// type `https://github.com/owner/repo`, and clone URLs carry or omit a
// trailing `.git`. Comparing them raw would treat one repository as several,
// which is the failure repository identity exists to prevent.
//
// Canonical form is `<lowercase host>/<owner>/<repo>`: no scheme, no user, no
// port, no trailing `.git`, no query string and no fragment. Only the host is
// lower-cased — owner and repo keep their original case, since a path that
// differs in case is a different string on disk and guessing would invent a
// match nobody asserted.
//
// What is stripped: the userinfo on every branch (`https://user:tok@host/…`,
// `user@host:path`, and the scheme-less `user@host/path` and
// `user:tok@host/path`), the query string and the fragment. A clone URL carries
// an inline token in those positions, and this column is replayed into every
// later session's digest and written into every export, so none of them may
// reach it. Every write of the column passes through this one function.
//
// What is not stripped: a token written into the path itself, which looks like
// any other path segment. That is the credential guard's job —
// CheckImportedProject judges repo_remote with the name and path. The store's
// project-creating INSERT arms ask it, the refresh and bind routes do not, and
// import and export always do: a token-shaped remote, however it was stored,
// is refused on import and named and left out of an export. A row an older
// build stored, query string and all, is not rewritten in place (there is no
// migration); it gets the same treatment while it is token-shaped.
//
// Returns "" for anything that is not a recognizable remote, so callers read
// "" as "no repository" rather than as a repository with an empty name.
func NormalizeRepoRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// A relative path with dot segments is not a repository URL. Without this
	// guard, ../../tmp/checkout becomes a canonical "remote" and suppresses
	// filesystem detection, so a second checkout silently fails to resolve.
	if isFilesystemPath(s) {
		return ""
	}

	// A query string and a fragment are not part of repository identity, and
	// both are positions a clone URL can carry an inline token in. Drop them
	// before any form is parsed, so no route — scheme-with-userinfo, scp, or a
	// bare host/path — can carry one into the stored column, and so an "@" in a
	// query is never mistaken for userinfo. This is the single function every
	// write of projects.repo_remote passes through; see the function comment for
	// why that matters.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}

	// scheme://[user@]host/path — the most explicit form.
	if i := strings.Index(s, "://"); i >= 0 {
		return canonicalRemote(dropAuthorityUserinfo(s[i+3:]))
	}

	// A scheme-less `user:token@host/path`. Its first colon sits BEFORE the
	// last "@" of the authority, which no scp-style remote has (there the colon
	// comes after the host), so it is recognised by that order and the userinfo
	// is cut before the scp split below can mistake `user` for a host and keep
	// `token@host/path` as the path.
	if auth, _, _ := strings.Cut(s, "/"); strings.Contains(auth, "@") {
		if c, at := strings.Index(auth, ":"), strings.LastIndex(auth, "@"); c >= 0 && c < at {
			s = s[at+1:]
		}
	}

	// scp-style [user@]host:path, where the colon separates host from path.
	// A Windows drive (C:\…) and a port (host:22/path) are not scp form.
	if host, path, ok := strings.Cut(s, ":"); ok && isScpHost(host) && path != "" {
		if at := strings.Index(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return canonicalRemote(host + "/" + path)
	}

	// Bare host/owner/repo, as a human would type it. The host must contain a
	// dot: a filesystem path like /home/u/ghost has no host at all, and
	// accepting it would invent a remote identity nobody asserted — turning
	// "unknown repository" into a confidently wrong one.
	// A `user@` in front of the host is cut here too (`git@github.com/owner/repo`
	// is the scp form typed with a slash), so no branch keeps userinfo.
	s = dropAuthorityUserinfo(strings.TrimPrefix(s, "/"))
	if host, _, ok := strings.Cut(s, "/"); ok && !strings.Contains(host, ".") {
		return ""
	}
	return canonicalRemote(s)
}

// dropAuthorityUserinfo removes `userinfo@` from the front of a `host/path`
// string. The authority is the text up to the first "/", and the userinfo ends
// at the LAST "@" in it, because a password may itself contain "@"; an "@"
// after the first "/" belongs to the path and is left alone.
func dropAuthorityUserinfo(s string) string {
	auth, _, _ := strings.Cut(s, "/")
	if at := strings.LastIndex(auth, "@"); at >= 0 {
		return s[at+1:]
	}
	return s
}

// isScpHost reports whether the text before a colon is an scp host rather than
// a drive letter or a port prefix.
func isScpHost(host string) bool {
	if host == "" || strings.Contains(host, "/") {
		return false
	}
	if len(host) == 1 { // drive letter
		return false
	}
	return true
}

// canonicalRemote assembles `<lowercase host>/<owner>/<repo>` from a
// host-and-path string. It requires both a host and a path: a remote with no
// path identifies no repository.
func canonicalRemote(s string) string {
	s = strings.TrimPrefix(s, "/")
	head, rest, ok := strings.Cut(s, "/")
	if !ok || head == "" || rest == "" {
		return ""
	}

	head = strings.ToLower(head)
	// Drop a port, but only from something that already looks like a hostname
	// — "github.com:22" and "github.com" are the same host.
	if h, _, hasPort := strings.Cut(head, ":"); hasPort && strings.Contains(h, ".") {
		head = h
	}
	if head == "" {
		return ""
	}

	// Trailing slashes are legal in remote URLs and must not survive into the
	// canonical form: ".../repo.git/" and ".../repo" are one repository, and
	// a difference here would silently split it back into two projects.
	rest = strings.TrimRight(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	if rest == "" {
		return ""
	}
	return head + "/" + rest
}

func isFilesystemPath(s string) bool {
	norm := strings.ReplaceAll(s, `\`, "/")
	if norm == "" {
		return false
	}
	if norm[0] == '/' || norm[0] == '~' {
		return true
	}
	if len(norm) >= 2 && norm[1] == ':' {
		return true // Windows drive-relative and drive-absolute paths
	}
	for _, part := range strings.Split(norm, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}
