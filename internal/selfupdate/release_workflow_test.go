package selfupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The producer side of the release-authenticity contract. `ghost upgrade`
// refuses to install a release it cannot attribute to this repository's
// release workflow (#694), which is only meaningful if the release actually
// publishes an attestation for every asset a user can download. The
// attestation itself is minted by actions/attest-build-provenance inside
// .github/workflows/release.yml, so nothing in the Go build fails when a step
// is deleted from that workflow — only this test does.
//
// These assertions are deliberately about the SHAPE of the workflow, not about
// whether GitHub accepted an upload: a release that silently stopped
// attesting would otherwise still pass every check in this package, because
// the client treats "no attestation" as an expected state for pre-cutover
// releases.

// attestAction is the action that mints the Sigstore bundle. Its ref is
// asserted separately because it is the one `uses:` in the release path that
// must never float on a mutable tag.
const attestAction = "actions/attest-build-provenance"

// workflowStep is one entry of a job's `steps:` list. `With` is typed as
// map[string]string because every value the attest action takes is a scalar.
type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

// workflowJob is one entry of the top-level `jobs:` map.
type workflowJob struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type releaseWorkflow struct {
	// Top-level `permissions:`. Nil when the file sets none, which is the
	// case today; the test asserts the attestation scopes are never granted
	// here, because a workflow-level grant would hand every job in the file
	// (including the ones that do not need it) an OIDC token.
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

// goreleaserArchive / goreleaserRelease mirror just enough of .goreleaser.yml
// to know which files land in the release's dist/ directory.
type goreleaserArchive struct {
	Formats         []string `yaml:"formats"`
	FormatOverrides []struct {
		GoOS    string   `yaml:"goos"`
		Formats []string `yaml:"formats"`
	} `yaml:"format_overrides"`
}

type goreleaserFile struct {
	Archives []goreleaserArchive `yaml:"archives"`
	Checksum struct {
		NameTemplate string `yaml:"name_template"`
	} `yaml:"checksum"`
	Release struct {
		ExtraFiles []struct {
			Glob string `yaml:"glob"`
		} `yaml:"extra_files"`
	} `yaml:"release"`
}

// shaPinPattern matches `actions/attest-build-provenance@<40 hex>`, i.e. a
// full-commit-SHA pin. Deliberately does not accept a tag or a branch ref.
var shaPinPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(attestAction) + `@[0-9a-f]{40}$`)

// versionCommentPattern matches the `# vX.Y.Z` trailer the repo puts after a
// SHA pin (see .github/workflows/reviewer.yml) so a human can resolve the
// digest back to a release tag. YAML strips the comment, so it is asserted
// against the raw file text.
var versionCommentPattern = regexp.MustCompile(`(?m)^([ \t-]*uses:[ \t]*` +
	regexp.QuoteMeta(attestAction) + `@[0-9a-f]{40})[ \t]+#[ \t]*(v[0-9]+\.[0-9]+\.[0-9]+)[ \t]*$`)

func attestationRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows", "release.yml")); err != nil {
		t.Fatalf("repo root %s has no .github/workflows/release.yml: %v", root, err)
	}
	return root
}

func loadReleaseWorkflow(t *testing.T) (releaseWorkflow, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(attestationRepoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	var wf releaseWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse release.yml: %v", err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("release.yml parsed with no jobs — the assertions below would pass vacuously")
	}
	return wf, string(raw)
}

func loadGoreleaser(t *testing.T) goreleaserFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(attestationRepoRoot(t), ".goreleaser.yml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yml: %v", err)
	}
	var g goreleaserFile
	if err := yaml.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse .goreleaser.yml: %v", err)
	}
	return g
}

// attestStepsByJob returns, per job, the indices of the steps that invoke the
// attest action. A job that does not attest is absent from the result, which
// is what the callers below compare against an expected job set.
func attestStepsByJob(wf releaseWorkflow) map[string][]int {
	found := map[string][]int{}
	for name, job := range wf.Jobs {
		for i, step := range job.Steps {
			if strings.HasPrefix(step.Uses, attestAction+"@") {
				found[name] = append(found[name], i)
			}
		}
	}
	return found
}

// subjectPaths returns the attest step's subject-path entries, one per line,
// with blank lines dropped. The action parses the input as a newline- (or
// comma-) separated list and globs each entry.
func subjectPaths(t *testing.T, step workflowStep) []string {
	t.Helper()
	raw, ok := step.With["subject-path"]
	if !ok {
		t.Fatalf("step %q uses %s but sets no subject-path", step.Name, attestAction)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("step %q sets an empty subject-path", step.Name)
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStringSets(t *testing.T, what string, got, want []string) {
	t.Helper()
	g, w := sortedCopy(got), sortedCopy(want)
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		t.Errorf("%s\n got: %v\nwant: %v", what, g, w)
	}
}

// TestReleaseWorkflowAttestsInExactlyTwoJobs pins the shape of the fix: the
// two jobs that publish downloadable assets attest, and no other job does.
// The `docker` job pushes a container image to ghcr.io and holds no
// attestation scope, so widening it would hand an unrelated job an OIDC
// token for no gain.
func TestReleaseWorkflowAttestsInExactlyTwoJobs(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	var attesting []string
	for name := range byJob {
		attesting = append(attesting, name)
	}
	equalStringSets(t,
		"jobs invoking "+attestAction+" — every job that publishes a downloadable asset must attest exactly once, and no others may",
		attesting, []string{"plugin", "release"})

	for job, idxs := range byJob {
		if len(idxs) != 1 {
			t.Errorf("job %q invokes %s %d times, want exactly 1", job, attestAction, len(idxs))
		}
	}
}

// TestAttestActionIsSHAWithVersionComment is the supply-chain check. The attest
// action is the one step that mints a statement of provenance that clients
// will trust, so a floating tag on it is the exact hole #694 is about. The
// version comment is what makes the digest reviewable: it names the release
// the commit belongs to, so a human can re-resolve the tag.
func TestAttestActionIsSHAWithVersionComment(t *testing.T) {
	wf, raw := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	seen := 0
	for job, idxs := range byJob {
		for _, i := range idxs {
			step := wf.Jobs[job].Steps[i]
			if !shaPinPattern.MatchString(step.Uses) {
				t.Errorf("job %q step %q pins %s by ref, want a full 40-character commit SHA\n  got: %s",
					job, step.Name, attestAction, step.Uses)
			}
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no " + attestAction + " invocation found — assertions above would pass vacuously")
	}

	matches := versionCommentPattern.FindAllStringSubmatch(raw, -1)
	if len(matches) != seen {
		t.Errorf("found %d `%s@<sha> # vX.Y.Z` pin lines, want %d (one per attest step)",
			len(matches), attestAction, seen)
	}
	for _, m := range matches {
		t.Logf("pinned %s # %s", m[1], m[2])
	}
}

// TestAttestationPermissionsAreScopedToTheJobsThatNeedThem checks the two
// scopes the attest action consumes — `attestations: write` to store the
// bundle and `id-token: write` to mint the OIDC token the Fulcio
// certificate is issued against — are held only by the attesting jobs, and
// never at the workflow level where `docker` would inherit them.
func TestAttestationPermissionsAreScopedToTheJobsThatNeedThem(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	if len(byJob) == 0 {
		t.Fatal("no job invokes " + attestAction + " — this test would pass on a release that attests nothing")
	}

	for _, scope := range []string{"attestations", "id-token"} {
		if got := wf.Permissions[scope]; got != "" {
			t.Errorf("workflow-level permissions grant %s: %s — that would hand the docker job an OIDC token it does not need", scope, got)
		}
	}

	for name, job := range wf.Jobs {
		_, attests := byJob[name]
		for _, scope := range []string{"attestations", "id-token"} {
			got := job.Permissions[scope]
			if attests && got != "write" {
				t.Errorf("job %q attests but does not hold %s: write (got %q)", name, scope, got)
			}
			if !attests && got != "" {
				t.Errorf("job %q does not attest but holds %s: %s", name, scope, got)
			}
		}
	}
}

// TestReleaseJobAttestsExactlyWhatGoReleaserPublishes is the coverage
// assertion, and the one that fails the loudest if the release config grows.
// The expected set is DERIVED from .goreleaser.yml rather than hardcoded, so
// adding a seventh archive format, renaming checksums.txt or adding a
// second extra file is a test failure until the attest step covers it.
// Both directions are compared: a subject path that is not a release asset
// is as much a drift as a release asset with no attestation.
func TestReleaseJobAttestsExactlyWhatGoReleaserPublishes(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	idxs, ok := byJob["release"]
	if !ok {
		t.Fatal("job \"release\" does not attest")
	}
	got := subjectPaths(t, wf.Jobs["release"].Steps[idxs[0]])

	g := loadGoreleaser(t)
	want := goReleaserSubjects(g)
	if len(want) == 0 {
		t.Fatal("derived no release subjects from .goreleaser.yml — the comparison below would pass vacuously")
	}
	equalStringSets(t,
		"release job subject-path vs the files .goreleaser.yml publishes",
		got, want)
}

// goReleaserSubjects derives the dist/-relative subject paths that cover every
// file .goreleaser.yml uploads as a release asset. A template segment becomes
// a `*` glob, because the version is only known when the tag is pushed.
func goReleaserSubjects(g goreleaserFile) []string {
	seen := map[string]bool{}
	add := func(p string) { seen[p] = true }

	if name := templateToGlob(g.Checksum.NameTemplate); name != "" {
		add("dist/" + name)
	}
	for _, archive := range g.Archives {
		for _, format := range archive.Formats {
			add("dist/*." + format)
		}
		for _, override := range archive.FormatOverrides {
			for _, format := range override.Formats {
				add("dist/*." + format)
			}
		}
	}
	for _, extra := range g.Release.ExtraFiles {
		if extra.Glob != "" {
			add("dist/" + extra.Glob)
		}
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// templateToGlob turns a GoReleaser name template into a glob: `{{.Version}}`
// and friends become `*`. A template with no segments is returned unchanged,
// which is the case for `checksum.name_template: checksums.txt` today.
func templateToGlob(template string) string {
	replaced := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(template, "*")
	// GoReleaser templates are `.tar.gz`-style suffixes too; a literal path
	// with no metacharacters is its own glob.
	return replaced
}

// TestPluginJobAttestsExactlyWhatItUploads closes the same loop for the three
// Claude Code plugin zips. Here the expected list is the literal upload list
// from the job's own `gh release upload` step, read out of the run script, so
// a fourth archive added to the upload without an attestation is a failure
// and not a silent gap.
func TestPluginJobAttestsExactlyWhatItUploads(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	idxs, ok := byJob["plugin"]
	if !ok {
		t.Fatal("job \"plugin\" does not attest")
	}
	got := subjectPaths(t, wf.Jobs["plugin"].Steps[idxs[0]])

	uploaded := uploadedPaths(t, wf.Jobs["plugin"].Steps, "gh", "release", "upload")
	if len(uploaded) == 0 {
		t.Fatal("no `gh release upload` step found in the plugin job — the comparison below would pass vacuously")
	}
	equalStringSets(t,
		"plugin job subject-path vs the assets it uploads to the release",
		got, uploaded)
}

// uploadedPaths collects the asset paths a `gh release upload` step passes.
// The command is a single logical line spread over several physical lines by
// backslash continuations, so the body is flattened first and then tokenised;
// flags, line continuations and the tag argument are dropped. The tag is the
// first non-flag argument after the command words, so the paths are collected
// by their `dist/` prefix rather than by position.
func uploadedPaths(t *testing.T, steps []workflowStep, command ...string) []string {
	t.Helper()
	words := len(command)
	var out []string
	for _, step := range steps {
		if !strings.Contains(step.Run, strings.Join(command, " ")) {
			continue
		}
		// Drop comments, then join continuations so the command is one line.
		var kept []string
		for _, line := range strings.Split(step.Run, "\n") {
			if idx := strings.Index(line, "#"); idx >= 0 {
				line = line[:idx]
			}
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				kept = append(kept, trimmed)
			}
		}
		fields := strings.Fields(strings.Join(kept, " "))
		for len(fields) > 0 && fields[len(fields)-1] == "\\" {
			fields = fields[:len(fields)-1]
		}
		if len(fields) < words || !equalFields(fields[:words], command) {
			continue
		}
		for _, field := range fields[words:] {
			if strings.HasPrefix(field, "-") {
				continue
			}
			if !strings.HasPrefix(field, "dist/") {
				// The tag being uploaded to, and anything else non-path.
				continue
			}
			out = append(out, field)
		}
	}
	return out
}

func equalFields(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestAttestationRunsBeforeTheReleaseIsPublished pins the ordering that makes
// a failed attestation recoverable. The plugin job's publish step
// (`gh release edit --draft=false`) is the point of no return, so the
// attestation must land before it: a failure in between leaves an
// unpublished draft that re-running the job fixes, exactly as the job's own
// failure-ordering comment above describes. Attesting after the publish would
// leave a public release whose plugin zips have no attestation, and the
// re-run would not repair it.
func TestAttestationRunsBeforeTheReleaseIsPublished(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	idxs, ok := byJob["plugin"]
	if !ok {
		t.Fatal("job \"plugin\" does not attest")
	}
	attest := idxs[0]
	publish := -1
	upload := -1
	for i, step := range wf.Jobs["plugin"].Steps {
		if step.Run != "" && strings.Contains(step.Run, "--draft=false") && publish == -1 {
			publish = i
		}
		if step.Run != "" && strings.Contains(step.Run, "gh release upload") && upload == -1 {
			upload = i
		}
	}
	if publish == -1 {
		t.Fatal("no publish step (`--draft=false`) found in the plugin job")
	}
	if upload == -1 {
		t.Fatal("no `gh release upload` step found in the plugin job")
	}
	if attest <= upload {
		t.Errorf("attest step is at %d, at or before the upload step at %d; the zips must exist as release assets first — see #694", attest, upload)
	}
	if attest >= publish {
		t.Errorf("attest step is at %d, at or after the publish step at %d; a failure between them would leave a public release with un-attested plugin zips", attest, publish)
	}
}

// TestReleaseJobAttestsAfterGoReleaser is the same ordering rule for the
// goreleaser job: the archives and checksums.txt do not exist until
// GoReleaser has run.
func TestReleaseJobAttestsAfterGoReleaser(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	idxs, ok := byJob["release"]
	if !ok {
		t.Fatal("job \"release\" does not attest")
	}
	attest := idxs[0]

	goreleaser := -1
	for i, step := range wf.Jobs["release"].Steps {
		if strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@") {
			goreleaser = i
			break
		}
	}
	if goreleaser == -1 {
		t.Fatal("no goreleaser step found in the release job")
	}
	if attest <= goreleaser {
		t.Errorf("attest step is at %d, at or before the GoReleaser step at %d; the archives do not exist yet", attest, goreleaser)
	}
}
