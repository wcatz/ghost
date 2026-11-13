package mcpinit

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
