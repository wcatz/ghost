package ai

import (
	"errors"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const harnessFailureOutputMax = 1200

func harnessFailureOutput(stdout, stderr string) string {
	var full string
	if t := strings.TrimSpace(stderr); t != "" {
		full = t
	} else {
		full = strings.TrimSpace(stdout)
	}
	if full == "" {
		return ""
	}
	if refusal := memory.RejectSecret("harness output", full); refusal != nil {
		var detected *memory.SecretContentError
		if errors.As(refusal, &detected) {
			return "<harness output withheld: it holds a " + detected.Format + ">"
		}
		return "<harness output withheld: it holds a credential>"
	}
	return tail(full, harnessFailureOutputMax)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return s[cut:]
}
