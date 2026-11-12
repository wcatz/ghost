package mcpinit

import "github.com/wcatz/ghost/internal/procstat"

func processStartTime(pid int) (string, bool) { return procstat.StartTime(pid) }

func isProcessAlive(pid int, wantToken string, haveToken bool) bool {
	return procstat.IsAlive(pid, wantToken, haveToken)
}
