package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// Link is one edge the store holds, as the arc grade reads it: source supersedes
// target, and an invalidated edge stands for nothing (supersede drops edges
// rather than deleting the row).
type Link struct {
	Source, Target, Relation string
	Invalidated              bool
}

// Stamp is what the store says about one row's age and liveness. created_at
// carries the storyline's chronology, resolved_at is the mark `ghost resolve`
// leaves on evidence it confirmed.
type Stamp struct {
	CreatedAt  string
	ResolvedAt string
}

// State is the graded end state of the run's own scratch store: the edges the arc
// stages wrote and the stamps they left. Grading reads the ROWS, not the CLI's
// prose, so a check cannot be passed or failed by a reworded output line.
type State struct {
	Links  []Link
	Stamps map[string]Stamp
}

// Ghost is the ghost surface a storyline run drives. Every method is one real
// ghost invocation or one read of the run's scratch store, so a test fake
// implements the whole surface without a binary and without a model.
type Ghost interface {
	// Bind records the session's working directory as the project's checkout. The
	// session-start block resolves a project from a directory, so a project that
	// records no location renders no project half and every session reads as a
	// first session.
	Bind(ctx context.Context, project, workDir string) error
	// Save writes one record through the real MCP tool — the genuine save path,
	// embedding-worker notification included.
	Save(ctx context.Context, project string, rec Record) (string, error)
	// Context renders the session-start injection block for a working directory:
	// the real `ghost context`, which is what the opencode plugin hands a session
	// and what the SessionStart hook renders for the same cwd.
	Context(ctx context.Context, workDir string) (string, error)
	// Settle waits until the store has caught up with the seeds (embeddings
	// drained) before the arc stages read it, then releases the write session.
	Settle(ctx context.Context) error
	// Supersede and Resolve are the two arc stages, run against the run's own
	// project. Each returns the command's stdout, which the report keeps.
	Supersede(ctx context.Context, project string) (string, error)
	Resolve(ctx context.Context, project string) (string, error)
	// Restamp rewrites created_at/updated_at so the order the records were seeded
	// in becomes true chronological order, one minute apart.
	Restamp(ctx context.Context, order []string) error
	State(ctx context.Context) (State, error)
}

// Session is one simulated session and everything the run can say about it.
type Session struct {
	Index  int
	Block  string // the injection this session received
	Prompt string
	Answer string
	// Saved maps the record keys this stage wrote to the ids the store gave them,
	// in seed order.
	Saved map[string]string
}

// Result is a completed run: every session, the arc stages' raw output, the end
// state, and the graded checks.
type Result struct {
	Story   Storyline
	WorkDir string
	// Opening carries the ids the OPENING records were saved under, keyed by
	// record key, on the same terms as a session's Saved. They are saved before
	// any session runs, so they belong to no session — and Validate lets a
	// storyline reverse an opening record, which makes the supersede-edge check
	// need their ids. Holding them here rather than in a local slice is what
	// keeps that lookup from failing on a store that got the edge right.
	Opening    map[string]string
	Sessions   []Session
	Supersede  string
	Resolve    string
	FinalBlock string
	State      State
	Checks     []Check
	// Judged records whether the opt-in judge ran, and Verdict is its raw answer.
	// An unreadable verdict is recorded as a failed ADVISORY check naming it
	// (never read as a yes or a no): the judge is a second column, and one reply
	// that is neither word must not discard the run's deterministic grade.
	Judged  bool
	Verdict string
	// WithoutGhost marks the control arm: the same scripts, model and saves, with
	// the session block replaced by an empty one. Only the answer lines are graded
	// (see grade), and the arm's verdict is a measurement, not a finding.
	WithoutGhost bool
}

// Arm names the arm a result came from, for reports and summaries.
func (r *Result) Arm() string {
	if r.WithoutGhost {
		return armWithoutGhost
	}
	return armWithGhost
}

const (
	armWithGhost    = "with-ghost"
	armWithoutGhost = "without-ghost"
)

// Passed reports whether every GATING check passed; an advisory check (the
// judge) is reported but never decides. A run with no checks has graded nothing
// and does not pass.
func (r *Result) Passed() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.Passed && !c.Advisory {
			return false
		}
	}
	return true
}

// FailedNames lists the checks that did not pass, for the report's summary.
func (r *Result) FailedNames() []string {
	var out []string
	for _, c := range r.Checks {
		if !c.Passed && !c.Advisory {
			out = append(out, c.Name)
		}
	}
	return out
}

// idOf resolves a storyline record key to the id the run's own save path gave it.
// It is the ONLY bridge from the annotation language (keys) to the store language
// (ids), and it reads the ids the saves returned rather than the store, so an id
// the runner lost shows up as a failed check instead of being quietly re-derived.
func (r *Result) idOf(key string) string {
	if id, ok := r.Opening[key]; ok {
		return id
	}
	for _, s := range r.Sessions {
		if id, ok := s.Saved[key]; ok {
			return id
		}
	}
	return ""
}

// Run is one storyline against one scratch-isolated Ghost. Ghost and Agent are
// the two seams; everything else here is the sequencing, and the sequencing is
// what the module is measuring.
type Run struct {
	Story   Storyline
	WorkDir string
	Ghost   Ghost
	Agent   Agent
	// Judge is opt-in and nil in a default run: the deterministic checks are the
	// grade, and a check nothing measured is not a check.
	Judge Agent
	// WithoutGhost runs the control arm: every record is still saved and the
	// project still bound, exactly as in the Ghost arm, but each session is handed
	// an EMPTY block in place of the real one. The injection framing stays in the
	// prompt, so the arm differs by the block's contents and by nothing else — an
	// empty block, not a missing hook. The arc stages and the end-state reads are
	// skipped, because no session of the arm is told what they produce.
	WithoutGhost bool
	// Timeout bounds one harness call and one arc stage. Zero means defaultTimeout.
	Timeout time.Duration
	Out     io.Writer
}

const (
	defaultTimeout = 10 * time.Minute
	// recordImportance is what every seeded record is saved at. A storyline is
	// about what carries forward, and a memory nobody considered worth
	// remembering would make that a question about importance instead.
	recordImportance = 0.7
)

// Execute runs the arc: open, one session per stage, settle, the two arc stages,
// the end state, and the grade.
func (r *Run) Execute(ctx context.Context) (*Result, error) {
	if r.Ghost == nil || r.Agent == nil {
		return nil, fmt.Errorf("run needs both a ghost and a harness")
	}
	if err := r.Story.Validate(); err != nil {
		return nil, err
	}
	if r.Timeout <= 0 {
		r.Timeout = defaultTimeout
	}
	res := &Result{Story: r.Story, WorkDir: r.WorkDir, WithoutGhost: r.WithoutGhost}

	// The project's own history first, then the location its sessions stand in.
	// Both happen before the first block is rendered: a project with no recorded
	// path renders no project half at all, and the first session would read as an
	// empty one for a reason that has nothing to do with the storyline.
	order := []string{}
	res.Opening = make(map[string]string, len(r.Story.Opening))
	for _, rec := range r.Story.Opening {
		id, err := r.save(ctx, rec)
		if err != nil {
			return nil, fmt.Errorf("opening record %s: %w", rec.Key, err)
		}
		order = append(order, id)
		// Kept on the Result, not only in the local order slice: a storyline may
		// reverse an opening record, and the grade resolves the edge through both
		// keys. See Result.Opening.
		res.Opening[rec.Key] = id
	}
	if err := r.Ghost.Bind(ctx, r.Story.Project, r.WorkDir); err != nil {
		return nil, fmt.Errorf("bind %s to %s: %w", r.Story.Project, r.WorkDir, err)
	}

	for i, stage := range r.Story.Stages {
		// The block is rendered BEFORE this stage's records exist, so what the
		// session received is exactly what the earlier sessions left behind.
		//
		// The control arm renders nothing: the records are all saved above and
		// below, so the store is the one the Ghost arm has, and the session is
		// handed an empty block inside the same prompt framing.
		var block string
		if !r.WithoutGhost {
			var err error
			block, err = r.call(ctx, func(c context.Context) (string, error) {
				return r.Ghost.Context(c, r.WorkDir)
			})
			if err != nil {
				return nil, fmt.Errorf("stage %d injection: %w", i+1, err)
			}
		}
		prompt := stagePrompt(stage.Script, block)
		answer, err := r.call(ctx, func(c context.Context) (string, error) {
			return r.Agent.Ask(c, prompt)
		})
		if err != nil {
			return nil, fmt.Errorf("stage %d session: %w", i+1, err)
		}
		sess := Session{Index: i, Block: block, Prompt: prompt, Answer: answer, Saved: map[string]string{}}
		for _, rec := range stage.Records {
			id, err := r.save(ctx, rec)
			if err != nil {
				return nil, fmt.Errorf("stage %d record %s: %w", i+1, rec.Key, err)
			}
			sess.Saved[rec.Key] = id
			order = append(order, id)
		}
		res.Sessions = append(res.Sessions, sess)
		_, _ = fmt.Fprintf(r.Out, "stage %d/%d: %d injected bytes, %d answer bytes, %d record(s) recorded\n",
			i+1, len(r.Story.Stages), len(block), len(answer), len(stage.Records))
	}

	if !r.WithoutGhost {
		if err := r.arcStages(ctx, res, order); err != nil {
			return nil, err
		}
	}
	res.Checks = grade(res)
	if r.Judge != nil {
		verdict, err := r.judge(ctx, res)
		if errors.Is(err, errNoJudgeTarget) {
			return nil, err
		}
		if err != nil {
			// A judge that could not be asked must not discard a graded run.
			res.Checks = append(res.Checks, Check{Name: "judge:error", Detail: err.Error(), Advisory: true})
			return res, nil
		}
		res.Judged, res.Verdict = true, verdict
		ok, err := judgeVerdict(verdict)
		if err != nil {
			res.Checks = append(res.Checks, Check{Name: "judge:unreadable", Detail: err.Error(), Advisory: true})
			return res, nil
		}
		res.Checks = append(res.Checks, judgedCheck(ok, verdict))
	}
	return res, nil
}

// arcStages is everything after the last session in the Ghost arm: restamp,
// settle, the two arc stages, and the end state the store checks grade.
func (r *Run) arcStages(ctx context.Context, res *Result, order []string) error {
	// Chronology is restamped once, over the whole arc, so the supersedes
	// direction between two records seeded in the same second is the storyline's
	// order rather than an arbitrary tie-break.
	if err := r.Ghost.Restamp(ctx, order); err != nil {
		return fmt.Errorf("restamp chronology: %w", err)
	}
	if err := r.callDo(ctx, r.Ghost.Settle); err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	sup, err := r.call(ctx, func(c context.Context) (string, error) {
		return r.Ghost.Supersede(c, r.Story.Project)
	})
	if err != nil {
		return fmt.Errorf("supersede: %w", err)
	}
	res.Supersede = sup

	// Resolve runs after supersede and before the end state is read: an edge the
	// demotion stage wrote is what the final block is supposed to stop carrying.
	resOut, err := r.call(ctx, func(c context.Context) (string, error) {
		return r.Ghost.Resolve(c, r.Story.Project)
	})
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	res.Resolve = resOut

	if res.FinalBlock, err = r.call(ctx, func(c context.Context) (string, error) {
		return r.Ghost.Context(c, r.WorkDir)
	}); err != nil {
		return fmt.Errorf("final injection: %w", err)
	}
	if res.State, err = r.callState(ctx); err != nil {
		return fmt.Errorf("graded state: %w", err)
	}
	return nil
}

func (r *Run) save(ctx context.Context, rec Record) (string, error) {
	return r.call(ctx, func(c context.Context) (string, error) {
		return r.Ghost.Save(c, r.Story.Project, rec)
	})
}

// call bounds one ghost or harness call with the run's own deadline. The CLI LLM
// clients apply a short default timeout only when the caller's context carries
// none, so a stage that needs longer has to be given one here rather than left to
// a default that would abort a legitimate run.
func (r *Run) call(ctx context.Context, fn func(context.Context) (string, error)) (string, error) {
	c, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	return fn(c)
}

// callDo is call for the one call that produces nothing to keep: the settling.
// It is bounded identically — a drain that hangs would hold the run open past
// every deadline the caller set on the harness.
func (r *Run) callDo(ctx context.Context, fn func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	return fn(c)
}

// callState reads the end state under the same deadline. It is its own function
// only because State answers with a State rather than with text.
func (r *Run) callState(ctx context.Context) (State, error) {
	c, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	return r.Ghost.State(c)
}

// defaultJudgeQuestion is the reversal arc's question; the other arcs name their
// own in Storyline.Judge. Its two verbs take the expected record's mark and
// content.
// errNoJudgeTarget is a storyline that cannot be judged: a configuration
// error that ends the run, unlike a judge call that merely failed.
var errNoJudgeTarget = errors.New("judge")

const defaultJudgeQuestion = "Did the session act on the current decision (%s: %s) rather than the one it replaced?"

// judge asks the harness whether the final session acted on the record it was
// expected to carry, and returns its raw answer. It is the paraphrase column: the
// deterministic answer lines match spellings, and a paraphrase of the claim
// passes this one and not those. Its check is advisory. The prompt states the
// rule the verdict has to follow: the session's own words, not the store's
// contents.
func (r *Run) judge(ctx context.Context, res *Result) (string, error) {
	last := res.Sessions[len(res.Sessions)-1]
	// The judge is asked about the final stage's expected record, and Validate
	// never requires one — it only requires that an Expect name an EARLIER
	// stage's record. An empty final Expect is therefore a valid storyline with
	// nothing to judge, and it has to be refused here rather than indexed into:
	// a panic this deep in a run is a crash after three model sessions spent.
	final := res.Story.Stages[len(res.Story.Stages)-1]
	if len(final.Expect) == 0 {
		return "", fmt.Errorf("%w: the final stage of %s expects no record, so there is no reversal to ask about", errNoJudgeTarget, res.Story.Key)
	}
	reversal, ok := res.Story.RecordByKey(final.Expect[0])
	if !ok {
		return "", fmt.Errorf("%w: the final stage expects no record in this storyline", errNoJudgeTarget)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are grading one session of a coding agent.\n\n"+
		"This is the whole of what that session was given:\n\n%s\n\n"+
		"This is what the session answered:\n\n%s\n\n",
		last.Prompt, last.Answer)
	question := res.Story.Judge
	if question == "" {
		question = defaultJudgeQuestion
	}
	fmt.Fprintf(&b, question+"\n"+
		"Answer with the single word yes or no, then at most one sentence of justification. "+
		"Judge only what the answer says — the injected block is the session's evidence, not proof it used it.\n",
		reversal.Mark, reversal.Content)
	return r.call(ctx, func(c context.Context) (string, error) {
		return r.Judge.Ask(c, b.String())
	})
}

// binaryGhost is the real Ghost: the ghost binary built from the checkout under
// test, running with this run's environment, plus one long-lived `ghost mcp`
// process for the saves (the embedding worker lives inside it, which is why the
// session has to stay open until Settle).
type binaryGhost struct {
	bin     string
	env     []string
	project string
	workDir string
	dbPath  string
	ollama  string
	drainTO time.Duration
	log     io.Writer
	mcp     *mcpSession
}

func newBinaryGhost(bin string, env []string, project, workDir, dbPath, ollama string, drainTO time.Duration, log io.Writer) *binaryGhost {
	return &binaryGhost{
		bin: bin, env: env, project: project, workDir: workDir, dbPath: dbPath,
		ollama: ollama, drainTO: drainTO, log: log,
	}
}

// start opens the MCP process the saves go through. Its stderr is the run's own
// log, never the child's stdout: stdout is the JSON-RPC stream.
func (g *binaryGhost) start(ctx context.Context) error {
	cmd := exec.Command(g.bin, "mcp")
	cmd.Env = g.env
	cmd.Stderr = g.log
	client := mcp.NewClient(&mcp.Implementation{Name: "ghost-evalstoryline", Version: "0.1.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect mcp: %w", err)
	}
	g.mcp = &mcpSession{sess: sess}
	return nil
}

// abort closes the MCP process for a run that ended before Settle did. Settle is
// the normal close; this is the one for a run that failed or was interrupted, so
// a failed run does not leave a ghost process holding the scratch store open.
func (g *binaryGhost) abort() {
	if g.mcp != nil {
		g.mcp.close()
	}
}

// Bind records the work dir as the project's checkout. project is the NAME the
// storyline speaks; `ghost project bind` takes an id, and a project created by
// `ghost_memory_save` under that name carries the same string as its id (the MCP
// save path derives the id from the name when the argument is not path-shaped), so
// one string addresses the project on both sides.
func (g *binaryGhost) Bind(ctx context.Context, project, workDir string) error {
	_, _, err := runGhost(ctx, g.env, g.bin, "project", "bind", project, workDir)
	return err
}

func (g *binaryGhost) Context(ctx context.Context, workDir string) (string, error) {
	out, errOut, err := runGhost(ctx, g.env, g.bin, "context", "--cwd", workDir)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(errOut) != "" {
		_, _ = fmt.Fprintf(g.log, "ghost context stderr: %s\n", strings.TrimSpace(errOut))
	}
	return out, nil
}

func (g *binaryGhost) Supersede(ctx context.Context, project string) (string, error) {
	out, _, err := runGhost(ctx, g.env, g.bin, arcPhaseArgs("supersede", project)...)
	return out, err
}

func (g *binaryGhost) Resolve(ctx context.Context, project string) (string, error) {
	out, _, err := runGhost(ctx, g.env, g.bin, arcPhaseArgs("resolve", project)...)
	return out, err
}

// arcPhaseArgs is the argv for the two arc stages that call a harness. --source
// is not optional: without it `ghost` resolves the harness by walking the process
// ancestry of whatever launched the runner, so a run started from inside Claude
// Code would have Claude classify an opencode session's reversal (and fail with
// "Not logged in" when the scratch holds only opencode's seeded credential). A
// run's harness is the one its sessions ran on, which is opencode, so the phases
// name it. Exposed as a function so the argv is testable without spawning.
func arcPhaseArgs(phase, project string) []string {
	return []string{phase, project, "--apply", "--source", "opencode"}
}

// savedIDRe extracts the memory id from a ghost_memory_save response
// ("Memory saved (id: <hex>), ...)", or "Global memory saved (id: ..." from
// ghost_save_global, hence the case-insensitive match). The real save path is the only way a
// storyline writes: a direct INSERT would skip the dedup, the credential guard
// and the embedding notification, and the grade would be of a store no session
// ever touched.
var savedIDRe = regexp.MustCompile(`(?i)memory saved \(id:\s*([0-9a-fA-F]+)\)`)

// saveCall is the tool and arguments one record is saved with. A global record
// goes through ghost_save_global, which takes no project, and a record with a
// valid_until passes it through as written. Exposed as a function so the routing
// is testable without a binary.
func saveCall(project string, rec Record) (string, map[string]any) {
	tags := rec.Tags
	if tags == nil {
		tags = []string{}
	}
	args := map[string]any{
		"content":    rec.Content,
		"category":   rec.Category,
		"importance": recordImportance,
		"tags":       tags,
	}
	if rec.ValidUntil != "" {
		args["valid_until"] = rec.ValidUntil
	}
	if rec.Global {
		return "ghost_save_global", args
	}
	args["project_id"] = project
	return "ghost_memory_save", args
}

func (g *binaryGhost) Save(ctx context.Context, project string, rec Record) (string, error) {
	if g.mcp == nil {
		return "", fmt.Errorf("mcp session is not open")
	}
	tool, args := saveCall(project, rec)
	out, err := g.mcp.callText(ctx, tool, args)
	if err != nil {
		return "", err
	}
	m := savedIDRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unrecognized %s response %q", tool, out)
	}
	return m[1], nil
}

// Settle waits for every project memory to have an embedding row before the arc
// stages read the store, then closes the MCP process. Supersede generates its
// candidate pairs from vectors, and a missing one is reported as "0 candidate
// pairs" rather than as an error — so an undrained store would grade the arc as
// a model failure. An unreachable Ollama fails here, loudly, instead.
func (g *binaryGhost) Settle(ctx context.Context) error {
	if g.mcp != nil {
		defer g.mcp.close()
	}
	if err := g.waitForEmbeddings(ctx); err != nil {
		return err
	}
	return nil
}

func (g *binaryGhost) waitForEmbeddings(ctx context.Context) error {
	if err := ollamaReachable(g.ollama); err != nil {
		return fmt.Errorf("embedding drain needs Ollama: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+g.dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	deadline := time.Now().Add(g.drainTO)
	for {
		var total, done int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories m JOIN projects p ON m.project_id=p.id WHERE p.name=? OR p.id=?`,
			g.project, g.project).Scan(&total); err != nil {
			return fmt.Errorf("count memories: %w", err)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memory_embeddings e JOIN memories m ON e.memory_id=m.id `+
				`JOIN projects p ON m.project_id=p.id WHERE p.name=? OR p.id=?`,
			g.project, g.project).Scan(&done); err != nil {
			return fmt.Errorf("count embeddings: %w", err)
		}
		if total > 0 && done == total {
			_, _ = fmt.Fprintf(g.log, "  embeddings drained: %d/%d\n", done, total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("embedding drain timeout: %d/%d embedded after %s", done, total, g.drainTO)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Restamp rewrites the timestamps so the seeding order is chronological order,
// one minute apart. created_at/updated_at carry second granularity, so a run that
// seeds a reversal a second after the claim it reverses leaves a tie and makes
// the edge between them arbitrary — which the direction check would then grade as
// a model failure. Timestamps are metadata: every record still arrived through
// the real save path.
func (g *binaryGhost) Restamp(_ context.Context, order []string) error {
	db, err := memory.OpenDB(g.dbPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	base := time.Now().UTC().Add(-time.Duration(len(order)) * time.Minute)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`UPDATE memories SET created_at=?, updated_at=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck
	for i, id := range order {
		ts := base.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05")
		if _, err := stmt.Exec(ts, ts, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// State reads the graded end state through a read-only handle: the run's own
// scratch store, opened the way every diagnostic read in this tree is opened, so
// the grade cannot itself write to what it is grading.
func (g *binaryGhost) State(ctx context.Context) (State, error) {
	db, err := sql.Open("sqlite", "file:"+g.dbPath+"?mode=ro")
	if err != nil {
		return State{}, err
	}
	defer db.Close() //nolint:errcheck
	project := g.project
	st := State{Stamps: map[string]Stamp{}}
	rows, err := db.QueryContext(ctx,
		`SELECT l.source_id, l.target_id, l.relation, l.invalidated_at
		 FROM memory_links l
		 JOIN memories s ON s.id = l.source_id
		 JOIN projects p ON s.project_id = p.id
		 WHERE p.name = ? OR p.id = ?
		 ORDER BY l.source_id, l.target_id, l.relation`, project, project)
	if err != nil {
		return State{}, fmt.Errorf("read links: %w", err)
	}
	for rows.Next() {
		var l Link
		var invalidated sql.NullString
		if err := rows.Scan(&l.Source, &l.Target, &l.Relation, &invalidated); err != nil {
			rows.Close() //nolint:errcheck
			return State{}, err
		}
		l.Invalidated = invalidated.Valid
		st.Links = append(st.Links, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return State{}, err
	}
	rows.Close() //nolint:errcheck

	mrows, err := db.QueryContext(ctx,
		`SELECT m.id, m.created_at, COALESCE(m.resolved_at, '') FROM memories m
		 JOIN projects p ON m.project_id = p.id
		 WHERE p.name = ? OR p.id = ? ORDER BY m.id`, project, project)
	if err != nil {
		return State{}, fmt.Errorf("read stamps: %w", err)
	}
	defer mrows.Close() //nolint:errcheck
	for mrows.Next() {
		var id string
		var s Stamp
		if err := mrows.Scan(&id, &s.CreatedAt, &s.ResolvedAt); err != nil {
			return State{}, err
		}
		st.Stamps[id] = s
	}
	return st, mrows.Err()
}

// mcpSession wraps one long-lived `ghost mcp` subprocess connected over stdio.
type mcpSession struct{ sess *mcp.ClientSession }

func (s *mcpSession) close() { _ = s.sess.Close() }

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func (s *mcpSession) callText(ctx context.Context, tool string, args map[string]any) (string, error) {
	res, err := s.sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s failed: %s", tool, textOf(res))
	}
	return textOf(res), nil
}

// ollamaReachable mirrors eval/cycle's gate verbatim: an unreachable embedding
// endpoint is reported as a missing dependency, not as a store with nothing to
// embed, because a drain that times out on zero vectors would be graded later as
// a model failure.
func ollamaReachable(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/api/tags")
	if err != nil {
		return fmt.Errorf("GET %s/api/tags: %w (is Ollama running?)", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama status %d", resp.StatusCode)
	}
	return nil
}
