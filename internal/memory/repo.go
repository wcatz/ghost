package memory

import "strings"

// NormalizeRepoRemote returns a canonical remote string, or "" if raw is not a recognizable remote.
func NormalizeRepoRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if isFilesystemPath(s) {
		return ""
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "://"); i >= 0 {
		return canonicalRemote(dropAuthorityUserinfo(s[i+3:]))
	}
	if auth, _, _ := strings.Cut(s, "/"); strings.Contains(auth, "@") {
		if c, at := strings.Index(auth, ":"), strings.LastIndex(auth, "@"); c >= 0 && c < at {
			s = s[at+1:]
		}
	}
	if host, path, ok := strings.Cut(s, ":"); ok && isScpHost(host) && path != "" {
		if at := strings.Index(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return canonicalRemote(host + "/" + path)
	}
	s = dropAuthorityUserinfo(strings.TrimPrefix(s, "/"))
	if host, _, ok := strings.Cut(s, "/"); ok && !strings.Contains(host, ".") {
		return ""
	}
	return canonicalRemote(s)
}

func dropAuthorityUserinfo(s string) string {
	auth, _, _ := strings.Cut(s, "/")
	if at := strings.LastIndex(auth, "@"); at >= 0 {
		return s[at+1:]
	}
	return s
}

func isScpHost(host string) bool {
	if host == "" || strings.Contains(host, "/") {
		return false
	}
	if len(host) == 1 {
		return false
	}
	return true
}

func canonicalRemote(s string) string {
	s = strings.TrimPrefix(s, "/")
	head, rest, ok := strings.Cut(s, "/")
	if !ok || head == "" || rest == "" {
		return ""
	}

	head = strings.ToLower(head)
	if h, _, hasPort := strings.Cut(head, ":"); hasPort && strings.Contains(h, ".") {
		head = h
	}
	if head == "" {
		return ""
	}

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
		return true
	}
	for _, part := range strings.Split(norm, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}
