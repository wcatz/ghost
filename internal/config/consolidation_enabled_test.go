package config

import "testing"

func TestReflectionConfig_ConsolidationEnabled_TruthTable(t *testing.T) {
	cases := []struct {
		name   string
		cfg    ReflectionConfig
		expect bool
	}{
		{"none", ReflectionConfig{}, false},
		{"reflect only", ReflectionConfig{AutoReflect: true}, true},
		{"resolve only", ReflectionConfig{AutoResolve: true}, true},
		{"supersede only", ReflectionConfig{AutoSupersede: true}, true},
		{"all three", ReflectionConfig{AutoReflect: true, AutoResolve: true, AutoSupersede: true}, true},
		{"reflect+resolve", ReflectionConfig{AutoReflect: true, AutoResolve: true}, true},
		{"resolve+supersede", ReflectionConfig{AutoResolve: true, AutoSupersede: true}, true},
		{"reflect+supersede", ReflectionConfig{AutoReflect: true, AutoSupersede: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.ConsolidationEnabled(); got != c.expect {
				t.Errorf("ConsolidationEnabled() = %v, want %v", got, c.expect)
			}
		})
	}
}
