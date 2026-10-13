package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestTransferArgRefusalsNeverEchoHostileArgv: the transfer commands' own
// argument errors render the offending argv through memory.RefusalArg, so a
// token-shaped or oversized argument is withheld.
func TestTransferArgRefusalsNeverEchoHostileArgv(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	values := map[string]string{"token": token, "long": strings.Repeat("Z", 300)}
	parsers := map[string]func([]string) error{
		"backup":        func(a []string) error { _, err := parseBackupArgs(a); return err },
		"backup verify": func(a []string) error { _, err := parseBackupVerifyArgs(a); return err },
		"export":        func(a []string) error { _, err := parseExportArgs(a); return err },
		"import":        func(a []string) error { _, err := parseImportArgs(a); return err },
	}
	for pname, parse := range parsers {
		for vname, v := range values {
			// A bare argument is a path to verify and to import, so only
			// backup and export refuse one; every parser refuses a flag.
			args := []string{"--" + v}
			if pname == "backup" || pname == "export" {
				args = append(args, v)
			}
			for i, arg := range args {
				t.Run(fmt.Sprintf("%s/%s/%d", pname, vname, i), func(t *testing.T) {
					err := parse([]string{arg})
					if err == nil {
						t.Fatalf("%q was accepted", arg[:10])
					}
					if strings.Contains(err.Error(), v) || strings.Contains(err.Error(), v[:40]) {
						t.Errorf("the error echoes the argument: %v", err)
					}
				})
			}
		}
	}
}
