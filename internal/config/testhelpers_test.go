package config

func resetWarned() {
	warned.Lock()
	defer warned.Unlock()
	warned.seen = map[string]struct{}{}
}
