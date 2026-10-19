package mcpserver

// #1541: ghost_list_projects and ghost_health grew with the number of projects
// and had no bound or cut notice. Both now fit the response budget the other
// read tools share, cutting whole project rows from the end of the listing.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// manyProjectsServer is a server over a store holding n path-shaped projects,
// the shape that made both answers large.
func manyProjectsServer(t *testing.T, n int) *Server {
	t.Helper()
	store, _ := testStoreWithPath(t)
	return projectsServerOver(t, store, n)
}

// projectsServerOver seeds n projects into a store the caller may already have
// written to, and serves it.
func projectsServerOver(t *testing.T, store *memory.Store, n int) *Server {
	t.Helper()
	for i := range n {
		path := fmt.Sprintf("/home/someone/src/organisation/projects/service-%03d", i)
		if err := store.EnsureProject(context.Background(), path, path, fmt.Sprintf("organisation/projects/service-%03d-with-a-long-descriptive-name", i)); err != nil {
			t.Fatalf("EnsureProject(%s): %v", path, err)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(store, logger, "test")
}

var projectsCutNotice = regexp.MustCompile(`\((\d+) rows were cut from the end of this answer to stay within the (\d+)-byte response budget`)

// projectsCutCount reads the number of rows the notice says were cut, or fails.
func projectsCutCount(t *testing.T, text string) int {
	t.Helper()
	m := projectsCutNotice.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("answer carries no cut notice (%d bytes):\n%.600s", len(text), text)
	}
	if want := strconv.Itoa(searchResponseMaxBytes); m[2] != want {
		t.Errorf("notice names budget %s, want %s", m[2], want)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestListProjectsIsBoundedWithACutNotice(t *testing.T) {
	srv := manyProjectsServer(t, 300)
	total := 301 // the 300 above and the fixture's own project
	text := ghostListProjectsText(t, srv)

	t.Logf("answer is %d bytes", len(text))
	if len(text) > searchResponseMaxBytes {
		t.Errorf("answer is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	shown := strings.Count(text, "\n- **")
	cut := projectsCutCount(t, text)
	if shown+cut != total {
		t.Errorf("shown %d + cut %d = %d, want %d projects", shown, cut, shown+cut, total)
	}
	if !strings.Contains(text, "No argument pages this list") {
		t.Errorf("notice does not say how to reach the rest:\n%s", text[len(text)-400:])
	}
	// Only the tail is cut: the rows kept are the first rows of the uncut
	// order, and the first project is among them.
	if !strings.HasPrefix(text, "## Ghost Projects\n\n- **") {
		t.Errorf("heading or first row changed:\n%.200s", text)
	}
}

func TestListProjectsFewProjectsIsUnchanged(t *testing.T) {
	srv := manyProjectsServer(t, 2)
	text := ghostListProjectsText(t, srv)
	if strings.Contains(text, "cut from") {
		t.Errorf("a small store got a cut notice:\n%s", text)
	}
	projects, err := srv.store.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	want.WriteString("## Ghost Projects\n\n")
	for _, p := range projects {
		n, err := srv.store.CountMemories(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&want, "- **%s** (id: `%s`, path: `%s`) — %d memories\n", p.Name, p.ID, p.Path, n)
	}
	if text != want.String() {
		t.Errorf("answer differs from the uncut render:\n got: %q\nwant: %q", text, want.String())
	}
}

func TestHealthIsBoundedAndKeepsStoreWideLines(t *testing.T) {
	srv := manyProjectsServer(t, 300)
	text := ghostHealthText(t, srv)

	t.Logf("answer is %d bytes", len(text))
	if len(text) > searchResponseMaxBytes {
		t.Errorf("answer is %d bytes, budget is %d", len(text), searchResponseMaxBytes)
	}
	shown := strings.Count(text, "\n- **")
	cut := projectsCutCount(t, text)
	if shown+cut != 301 {
		t.Errorf("shown %d + cut %d = %d, want 301 projects", shown, cut, shown+cut)
	}
	// The store-wide lines are never cut, and the total still covers every
	// project, the cut ones included.
	for _, want := range []string{
		"**Projects:** 301\n",
		"**Total memories:** 0\n",
		"**Embeddings:** disabled",
		"**Memory links:**",
		"**History:**",
		"Retrieval",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("store-wide line %q missing:\n%.800s", want, text)
		}
	}
}

func TestHealthTotalCountsProjectsThatAreCut(t *testing.T) {
	store, _ := testStoreWithPath(t)
	srv := projectsServerOver(t, store, 300)
	last := "/home/someone/src/organisation/projects/service-299"
	if _, err := store.CreateWithID(context.Background(), last, "A20E133860CC4AFE38B485AD5371BA59",
		memory.Memory{Content: "kept in a project the listing cuts", Category: "gotcha", Source: "manual"}); err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	text := ghostHealthText(t, srv)
	if projectsCutCount(t, text) == 0 || strings.Contains(text, last) {
		t.Fatalf("fixture error: the project holding the memory must be one the answer cuts")
	}
	if !strings.Contains(text, "**Total memories:** 1\n") {
		t.Errorf("total does not include the memory in a cut project:\n%.800s", text)
	}
}

func TestHealthFewProjectsHasNoNotice(t *testing.T) {
	srv := manyProjectsServer(t, 2)
	text := ghostHealthText(t, srv)
	if strings.Contains(text, "cut from") {
		t.Errorf("a small store got a cut notice:\n%s", text)
	}
	if got := strings.Count(text, "\n- **"); got != 3 {
		t.Errorf("shows %d project rows, want 3:\n%s", got, text)
	}
	// Order of sections is what it was: count line, rows, then the total.
	i, j, k := strings.Index(text, "**Projects:**"), strings.Index(text, "\n- **"), strings.Index(text, "**Total memories:**")
	if i >= j || j >= k {
		t.Errorf("sections out of order: %d %d %d", i, j, k)
	}
}
