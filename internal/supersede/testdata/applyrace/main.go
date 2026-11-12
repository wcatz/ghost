// Command applyrace runs a single apply pass for the concurrent-apply process test.
// It stops inside classification so the parent can interleave two passes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/supersede"
)

type report struct {
	Label         string      `json:"label"`
	Asked         [][2]string `json:"asked,omitempty"`
	Created       int         `json:"created"`
	Confirmed     int         `json:"confirmed"`
	Refused       int         `json:"refused"`
	Opposite      int         `json:"opposite_live"`
	CausesCreated int         `json:"causes_created"`
	CausesWritten int         `json:"causes_written"`
	Error         string      `json:"error,omitempty"`
}

const barrierWait = 60 * time.Second

func main() {
	var (
		db        = flag.String("db", "", "path to the shared ghost.db")
		project   = flag.String("project", "", "project name or id to pass over")
		barrier   = flag.String("barrier", "", "directory the barriers are files in")
		label     = flag.String("label", "", "this child's name in the barrier directory")
		threshold = flag.Float64("threshold", 0.9, "cosine threshold for a candidate pair")
		verdict   = flag.String("verdict", "supersedes", "the relation the classifier answers: supersedes or causes")
	)
	flag.Parse()

	rep := report{Label: *label}
	defer func() {
		enc := json.NewEncoder(os.Stdout)
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
			os.Exit(1)
		}
	}()

	if *db == "" || *barrier == "" || *label == "" {
		fail(&rep, "missing -db, -barrier or -label")
		return
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	handle, err := memory.OpenDB(*db)
	if err != nil {
		fail(&rep, fmt.Sprintf("open database: %v", err))
		return
	}
	store := memory.NewStore(handle, logger)
	defer store.Close() //nolint:errcheck

	answer, err := relationFor(*verdict)
	if err != nil {
		fail(&rep, err.Error())
		return
	}
	cls := &raceClassifier{barrier: *barrier, label: *label, verdict: answer}
	res, _, err := supersede.RunWith(ctx, store, cls, *project, supersede.Options{
		Threshold: float32(*threshold),
		Apply:     true,
	}, logger)
	rep.Created, rep.Confirmed, rep.Refused, rep.Opposite = res.Created, res.Confirmed, res.ReverseLive, res.OppositeLive
	rep.CausesCreated, rep.CausesWritten = res.CausesCreated, res.CausesWritten
	rep.Asked = cls.asked
	if err != nil {
		rep.Error = err.Error()
		fmt.Fprintf(os.Stderr, "apply pass failed: %v\n", err)
		os.Exit(1)
	}
}

type raceClassifier struct {
	barrier string
	label   string
	verdict supersede.Relation
	asked   [][2]string
}

func relationFor(name string) (supersede.Relation, error) {
	switch supersede.Relation(name) {
	case supersede.RelationSupersedes, supersede.RelationCauses:
		return supersede.Relation(name), nil
	}
	return "", fmt.Errorf("-verdict %q is not a relation this pass writes", name)
}

func (c *raceClassifier) ClassifyBatch(ctx context.Context, pairs []supersede.Candidate) ([]supersede.Relation, error) {
	out := make([]supersede.Relation, len(pairs))
	for i, p := range pairs {
		c.asked = append(c.asked, [2]string{p.NewerID, p.OlderID})
		out[i] = c.verdict
	}
	if err := c.signal("asked"); err != nil {
		return nil, err
	}
	if err := c.wait(ctx, "go"); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *raceClassifier) signal(name string) error {
	path := filepath.Join(c.barrier, c.label+"-"+name)
	if err := os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		return fmt.Errorf("signal barrier %s: %w", path, err)
	}
	return nil
}

func (c *raceClassifier) wait(ctx context.Context, name string) error {
	path := filepath.Join(c.barrier, c.label+"-"+name)
	deadline := time.Now().Add(barrierWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for barrier %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("barrier %s never appeared in %s", path, c.barrier)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func fail(rep *report, why string) {
	rep.Error = why
	fmt.Fprintf(os.Stderr, "%s\n", why)
	os.Exit(1)
}
