package mcpinit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type stopHookInput struct {
	TranscriptPath string `json:"transcript_path"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// transcriptLine is the minimal shape needed to spot tool_use entries in a
// Claude Code transcript JSONL line. Everything else in the line is ignored.
type transcriptLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// ghostSaveTools are the tool names whose presence in a transcript proves the
// session saved knowledge to Ghost.
var ghostSaveTools = map[string]bool{
	"mcp__ghost__ghost_memory_save": true,
	"mcp__ghost__ghost_save_global": true,
}

// stopBlockMessage is emitted (as hook JSON) when a tool-using session ends
// without a single Ghost save. Claude Code shows the reason to Claude and the
// session continues once; stop_hook_active guarantees the second stop wins.
const stopBlockMessage = `{"decision":"block","reason":"This session used tools but saved nothing to Ghost. Review the session for discoveries worth keeping (commands, configs, gotchas, decisions) and save them with ghost_memory_save — or stop again if there is truly nothing to save."}`

// HandleStopHook is invoked by Claude Code when a session stops via:
//
//	ghost hook stop
//
// It blocks the stop once — via {"decision":"block"} on stdout — when the
// session used tools but never saved anything to Ghost. Every failure path
// returns silently, allowing the stop: the hook must never trap a session.
// It performs no database access.
func HandleStopHook(stdin io.Reader, stdout io.Writer) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return
	}
	var input stopHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	// A prior block already fired this session — the second stop always wins.
	if input.StopHookActive {
		return
	}
	if input.TranscriptPath == "" {
		return
	}
	f, err := os.Open(input.TranscriptPath)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck

	toolCalls, ghostSaves := scanTranscript(f)
	if toolCalls == 0 || ghostSaves > 0 {
		return
	}
	_, _ = fmt.Fprintln(stdout, stopBlockMessage)
}

// scanTranscript streams a transcript and counts assistant tool_use blocks,
// plus how many were Ghost save tools. Unparseable lines are skipped; a
// scanner error mid-file yields the counts seen so far — worst case the nudge
// fires once and the second stop passes through the stop_hook_active guard.
func scanTranscript(r io.Reader) (toolCalls, ghostSaves int) {
	sc := bufio.NewScanner(r)
	// Transcript lines carry full tool results and can be huge.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var line transcriptLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		for _, c := range line.Message.Content {
			if c.Type == "tool_use" {
				toolCalls++
				if ghostSaveTools[c.Name] {
					ghostSaves++
				}
			}
		}
	}
	return toolCalls, ghostSaves
}
