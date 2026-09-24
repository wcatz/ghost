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
// port, no trailing `.git`. Only the host is lower-cased — owner and repo keep
// their original case, since a path that differs in case is a different string
// on disk and guessing would invent a match nobody asserted.
//
// Returns "" for anything that is not a recognizable remote, so callers read
// "" as "no repository" rather than as a repository with an empty name.
func NormalizeRepoRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// scheme://[user@]host/path — the most explicit form.
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		return canonicalRemote(rest)
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
	if host, _, ok := strings.Cut(strings.TrimPrefix(s, "/"), "/"); ok && !strings.Contains(host, ".") {
		return ""
	}
	return canonicalRemote(s)
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

	rest = strings.TrimSuffix(rest, ".git")
	if rest == "" {
		return ""
	}
	return head + "/" + rest
}

// RepoRemoteOwnerName splits a normalized remote into owner and repository
// name.
//
// Derived rather than stored: a project's remote can be rewritten (repository
// moved, mirror swapped, host renamed), and columns copied from it would drift
// out of step the first time that happened. The remote is the single fact;
// everything else is a function of it.
//
// Returns ("", "") unless the remote splits into a host plus at least one
// more segment, so a malformed value cannot produce a plausible-looking owner.
func RepoRemoteOwnerName(normalized string) (owner, repo string) {
	parts := strings.Split(strings.Trim(normalized, "/"), "/")
	if len(parts) < 3 {
		return "", ""
	}
	owner = parts[len(parts)-2]
	repo = parts[len(parts)-1]
	return owner, repo
}
