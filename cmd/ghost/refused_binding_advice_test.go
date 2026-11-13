package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The notice a refused repository binding returns is advice, and advice is a
// claim: it tells the reader to run two commands, and if the second one is
// missing then following it does not fix anything. These tests run what the
// notice says, through the same command bodies the CLI runs, and check the
// state it promises.
//
// A merge alone does not fix it. mergeProjectTx deletes the source project and
// never carries its repository across, so merging the project this save opened
// into the one that kept the name takes the repository with it: the next save
// from that checkout finds no project by remote, is refused by the recorded
// path all over again, and opens a second project. The bind is what records
// the checkout and its repository on the surviving project, and it only works
// after the merge, because while the other project still records that
// directory the bind is refused.

// adviceCommandRe finds the `ghost project <sub> <arg> [<arg>]` commands a
// notice names, in the order it names them. The notice shell-quotes its
// arguments (single quotes, with an embedded quote closed, escaped and
// reopened), so a path with a space in it is one argument rather than two and a
// Windows path keeps one set of backslashes. A bare word is the same shape
// minus the quotes, and covers the ids that need no quoting at all.
//
// The quoted alternative is an opening quote, then (any non-quote | an embedded quote
// spelled \\'\\' followed by more non-quotes)*, then a closing quote. The doubled
// backslashes are load-bearing: a backslash in a regexp pattern is an escape, so a single
// one there reads as a bare quote and the word would end at the first quote it met.
var adviceCommandRe = regexp.MustCompile(`ghost project (merge|bind) ('(?:[^']|'\\''[^']*)*'|\S+)(?: ('(?:[^']|'\\''[^']*)*'|\S+))?`)

type adviceCommand struct {
	sub  string
	args []string
}

// adviceCommands extracts the commands a notice names. A placeholder argument
// — <duplicate-id> and the like — is a command the notice asks the reader to
// fill in, and is reported as one so a test can say so rather than run it.
func adviceCommands(t *testing.T, notice string) []adviceCommand {
	t.Helper()
	var commands []adviceCommand
	for _, match := range adviceCommandRe.FindAllStringSubmatch(notice, -1) {
		cmd := adviceCommand{sub: match[1]}
		for _, raw := range match[2:] {
			if raw == "" {
				continue
			}
			if len(raw) >= 2 && strings.HasPrefix(raw, `'`) && strings.HasSuffix(raw, `'`) {
				// A POSIX single-quoted word: everything between the outer
				// quotes is literal except the `'\''` sequence, which
				// stands for one embedded quote.
				cmd.args = append(cmd.args, strings.ReplaceAll(raw[1:len(raw)-1], `'\''`, `'`))
				continue
			}
			cmd.args = append(cmd.args, raw)
		}
		commands = append(commands, cmd)
	}
	return commands
}

// runAdvice runs one command from a notice through the same body the CLI runs.
func runAdvice(t *testing.T, ctx context.Context, store *memory.Store, cmd adviceCommand, detectRemote func(string) string) error {
	t.Helper()
	var out bytes.Buffer
	switch cmd.sub {
	case "merge":
		if len(cmd.args) != 2 {
			t.Fatalf("advice command %q has %d arguments, want 2", cmd.sub, len(cmd.args))
		}
		return runProjectMergeCore(ctx, store, &out, cmd.args[0], cmd.args[1])
	case "bind":
		if len(cmd.args) != 2 {
			t.Fatalf("advice command %q has %d arguments, want 2", cmd.sub, len(cmd.args))
		}
		return runProjectBindCore(ctx, store, &out, cmd.args[0], cmd.args[1], detectRemote)
	default:
		t.Fatalf("advice names an unknown command %q", cmd.sub)
		return nil
	}
}

// refusedBindingNotice builds the path-mismatch refusal: a project named "infra"
// at a checkout of its own that claims no repository, and a second directory of
// the same basename that saves a memory of its own. It returns the store, the
// project that kept the name, the project the save went to, and the notice.
func refusedBindingNotice(t *testing.T) (store *memory.Store, held, saved, notice string) {
	t.Helper()
	const remote = "https://github.com/wcatz/infra.git"
	ctx := context.Background()

	// Both directories exist: the refusal compares them on disk, and a
	// directory that does not resolve would make this pass for the wrong
	// reason.
	root := t.TempDir()
	recorded := existingCheckout(t, root, "git", "infra")
	other := existingCheckout(t, root, "Downloads", "infra")

	store = bindStore(t)
	if err := store.EnsureProject(ctx, "real-infra", recorded, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	saved, refusal, err := store.ResolveOrCreateRepoProject(ctx, other, "infra", other, other, other, remote)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if refusal == nil {
		t.Fatalf("an unrelated clone saved into a project of the same name, want the refusal reported")
	}
	if _, _, _, err := store.Upsert(ctx, saved, "fact", "a fact saved from the clone", "manual", 0.5, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return store, "real-infra", saved, refusal.Notice()
}

// existingCheckout creates a directory under root and returns its physical
// path, which is the spelling a bind records and a comparison resolves to.
func existingCheckout(t *testing.T, root string, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{root}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	return physicalDir(t, dir)
}

// TestRefusedBindingAdviceMakesTheNextSaveStick is the end-to-end claim: a
// reader who runs the commands the notice names ends up with one project, the
// memory they had just saved in it, and the next save from that checkout
// landing there instead of opening a second project.
func TestRefusedBindingAdviceMakesTheNextSaveStick(t *testing.T) {
	const remote = "https://github.com/wcatz/infra.git"
	store, held, saved, notice := refusedBindingNotice(t)
	ctx := context.Background()

	commands := adviceCommands(t, notice)
	if len(commands) != 2 || commands[0].sub != "merge" || commands[1].sub != "bind" {
		t.Fatalf("notice names %+v, want a merge followed by a bind: %q", commands, notice)
	}
	if commands[0].args[0] != saved || commands[0].args[1] != held {
		t.Errorf("merge %v collects %q into %q, want the project this save used into the one that kept the name",
			commands[0].args, saved, held)
	}
	if commands[1].args[0] != held || commands[1].args[1] != saved {
		t.Errorf("bind %v records the checkout on %q, want the project that kept the name bound to this checkout",
			commands[1].args, held)
	}
	// A fake detector rather than repo.DetectRemote: the test must not spawn
	// git, and this is the same seam runProjectBindCore injects one for.
	detect := func(string) string { return remote }
	for _, cmd := range commands {
		if err := runAdvice(t, ctx, store, cmd, detect); err != nil {
			t.Fatalf("running the advice's %q: %v\nnotice: %s", cmd.sub, err, notice)
		}
	}

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != held {
		t.Fatalf("projects after the advice = %+v, want only %q", projects, held)
	}
	memories, err := store.GetAll(ctx, held, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(memories) != 1 || !strings.Contains(memories[0].Content, "a fact saved from the clone") {
		t.Errorf("the memory saved into %q did not follow the merge into %q: %+v", saved, held, memories)
	}

	// The point of the bind: the next save from that checkout reaches the
	// project that kept the name, and nothing is refused and nothing is opened.
	again, refused, err := store.ResolveOrCreateRepoProject(ctx, saved, "infra", saved, saved, saved, remote)
	if err != nil {
		t.Fatalf("the save after the advice failed: %v", err)
	}
	if refused != nil {
		t.Errorf("the save after the advice is still refused, so the advice did not last: %+v", refused)
	}
	if again != held {
		t.Errorf("the save after the advice resolved to %q, want %q", again, held)
	}
	after, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("the save after the advice opened %d projects, want the 1 that was already there: %+v", len(after), after)
	}
}
