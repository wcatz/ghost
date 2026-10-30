package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/secret"
)

// TestDisplayProposalNeverPrintsACredential covers every place `ghost reflect`
// echoes a proposal or a guarded drop back to the operator, in one helper.
//
// The report deliberately emits only format, category, scope and length for a
// proposal it refuses to store. That guarantee is worthless if the same command
// printed the value ninety lines earlier, and it did: the proposal listing and
// the drop-guard warning both printed 120 characters of stored content, and
// 120 is far more than a GitHub PAT or a Docker Hub token needs. In the
// autonomous path that stdout is the append-only lifecycle.log, so the value
// would outlive the run — which is the exposure the credential refusal exists
// to prevent, arrived at from the other direction.
//
// So the detection runs at every print site rather than at the write boundary.
// A boundary-only guarantee is not a guarantee about the command's report.
func TestDisplayProposalNeverPrintsACredential(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the
	// PAT format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + rep("a1B2c3D4e5F6", 3) + "AbCd"

	clean := "the relay listens on 2222"
	if got := displayProposal(clean, "fact", 120); got != assemble.PreviewLine(clean, 120) {
		t.Errorf("displayProposal(%q) = %q, want the first line of the content — a clean proposal must still be readable", clean, got)
	}

	got := displayProposal("the deploy token is "+credential, "fact", 120)
	if strings.Contains(got, credential) {
		t.Errorf("displayProposal printed the credential: %q", got)
	}
	if !strings.Contains(got, "withheld") || !strings.Contains(got, "GitHub personal access token") {
		t.Errorf("displayProposal did not name the format it withheld: %q", got)
	}
	// The category is still shown, so an operator can find the row.
	if !strings.Contains(got, "fact") {
		t.Errorf("displayProposal dropped the category: %q", got)
	}
	// And the length, so the operator can tell how much was withheld.
	if !strings.Contains(got, "bytes=") {
		t.Errorf("displayProposal dropped the length: %q", got)
	}
}

// TestDisplayProposalCoversEveryFlaggedShape is the corpus discipline applied
// to the report: the substitution must hold for every rule the detector has, not
// only the prefix rules a reviewer would think of.
func TestDisplayProposalCoversEveryFlaggedShape(t *testing.T) {
	shapes := []string{
		"the token is ghp_" + rep("a1B2c3D4e5F6", 3) + "AbCd",
		"postgres://ghost:hunter2isnotsafe@relay-1.example/ghost",
		"the cold key cborHex: 5840" + rep("f2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f", 2),
		"-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYL\n-----END RSA PRIVATE KEY-----",
		"password: Zq7Xn4Bt2Lm9Kc5Vr8Wd",
	}
	for _, s := range shapes {
		finding, ok := secret.Detect(s)
		if !ok {
			t.Errorf("fixture is not flagged, so it does not test the substitution: %q", s)
			continue
		}
		got := displayProposal(s, "fact", 120)
		if strings.Contains(got, "MIIBOGIB") || strings.Contains(got, "f2429ae1") || strings.Contains(got, "Zq7Xn4Bt2Lm9") ||
			strings.Contains(got, "hunter2isnotsafe") || strings.Contains(got, "a1B2c3D4e5F6") {
			t.Errorf("displayProposal leaked part of a %s value: %q", finding.Label, got)
		}
	}
}

func rep(s string, n int) string { return strings.Repeat(s, n) }

// TestRunReflectProjectHeaderRedactsTokenShapedProjectID drives runReflect
// with a project id shaped like a GitHub personal access token and asserts
// the phase header withholds it rather than printing it verbatim.
func TestRunReflectProjectHeaderRedactsTokenShapedProjectID(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	ctx := context.Background()
	const projectName = "reflect-project"
	cred := reflectCredential()

	if err := os.MkdirAll(filepath.Join(dataHome, "ghost"), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    path TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
)`); err != nil {
		t.Fatalf("create projects: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE memories (
    id TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category TEXT NOT NULL DEFAULT 'fact',
    content TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'mcp',
    importance REAL NOT NULL DEFAULT 0.5,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    scope TEXT NOT NULL DEFAULT '',
    pinned INTEGER NOT NULL DEFAULT 0,
    resolved_at TEXT
)`); err != nil {
		t.Fatalf("create memories: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, cred, "/tmp/"+projectName, projectName); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memories (project_id, category, content, source, importance) VALUES (?, 'fact', 'keep', 'mcp', 0.5)`, cred); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	out := captureStderr(t, func() {
		orig := os.Args
		os.Args = []string{orig[0], "reflect", cred, "--tier", "sqlite"}
		defer func() { os.Args = orig }()
		runReflect()
	})
	if strings.Contains(out, cred) {
		t.Fatalf("project header printed verbatim credential:\n%s", out)
	}
	if !strings.Contains(out, "<project withheld: it holds a GitHub personal access token>") {
		t.Fatalf("project header missing withheld rendering:\n%s", out)
	}
	if !strings.Contains(out, "Project:") {
		t.Fatalf("project header not printed:\n%s", out)
	}
	_ = ctx
}
