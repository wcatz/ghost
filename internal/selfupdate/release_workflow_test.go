package selfupdate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The producer side of the release-authenticity contract (#694). The client
// half — `ghost upgrade` verifying the Sigstore bundle this workflow mints
// before it unpacks anything — is not in this checkout: nothing in
// `internal/selfupdate` reads an attestation yet, and docs/cli.md says so. What
// this test guards is the half the client will depend on, namely that the
// release publishes a bundle for EVERY asset a user can download, under the
// SAN and with the materials the verifier needs.
//
// The attestation is minted by actions/attest-build-provenance inside
// .github/workflows/release.yml, so nothing in the Go build fails when a step
// is deleted from that workflow — only this test does.
//
// These assertions are deliberately about the SHAPE of the workflow, not about
// whether GitHub accepted an upload. A release that silently stopped attesting
// would leave every client-side check unsatisfiable, and the failure would
// surface to users as a refused upgrade rather than as a build break.

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
		// Draft and UseExistingDraft together are what make a job re-run adopt
		// the release it already created rather than making a second one;
		// ReplaceExistingArtifacts is what lets that re-run replace an asset
		// whose name is already taken. TestTheReleaseJobIsReRunnable asserts
		// the three together, because the third is only safe while the first
		// holds.
		Draft                    bool `yaml:"draft"`
		UseExistingDraft         bool `yaml:"use_existing_draft"`
		ReplaceExistingArtifacts bool `yaml:"replace_existing_artifacts"`
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

// goReleaserSubjects derives the subject paths that cover every file
// .goreleaser.yml uploads as a release asset. A template segment becomes a `*`
// glob, because the version is only known when the tag is pushed.
//
// The three kinds are at THREE different roots, and conflating them is a
// silently-empty glob rather than a test failure:
//
//   - archives and checksums.txt are GoReleaser OUTPUTS, written into its dist
//     directory, so they are `dist/`-prefixed;
//   - release.extra_files are files that already EXIST in the repository.
//     internal/extrafiles resolves each `glob` with fileglob.Glob from the
//     working directory, which is the project root, and uploads the matched
//     file from there under `filepath.Base(file)`. GoReleaser does not copy
//     them into dist/ first, so an extra file's subject path is the glob
//     verbatim — repo-root relative, with no prefix.
//
// That second case is why `dist/` + glob was wrong for install.ps1: the
// prefixed path names a file GoReleaser never creates, actions/attest
// (src/subject.ts, getSubjectFromPath) drops a pattern that matches nothing as
// long as another pattern matched, and the release published install.ps1 with
// no attestation while docs/installation.md said it had one.
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
		// Verbatim, and NOT prefixed: see the doc comment above. The leading
		// "./" GoReleaser's own documentation uses is stripped so the two
		// spellings of the same file compare equal.
		if glob := strings.TrimPrefix(extra.Glob, "./"); glob != "" {
			add(glob)
		}
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

// globMetaPattern matches a subject path that is a glob rather than a literal
// file name. It is deliberately the same character class the attest action's
// own getSubjectFromPath uses to decide whether to expand an entry.
var globMetaPattern = regexp.MustCompile(`[*?\[]`)

// TestEveryLiteralAttestationSubjectExists is the check the derivation cannot
// make. Deriving a subject path from .goreleaser.yml proves the LIST is right;
// it cannot prove the workflow spells it the way the derivation produced, because
// that is the very thing being compared. So this asserts the property directly:
// every subject path with no glob metacharacter must name a file that WILL
// EXIST when the attest step runs, and only three things in this repository
// qualify —
//
//   - a file in the repository, which is what release.extra_files points at;
//   - dist/<checksum name_template>, the one GoReleaser output whose name has no
//     template segment in it;
//   - a dist/ path the job's own earlier steps build (the plugin archives).
//
// Everything else fails. The tempting weaker rule — "a literal under dist/ is a
// build output, trust it" — is exactly the hole: `dist/install.ps1` is
// dist/-prefixed like a build output, so it sails through, and the only thing
// that catches it is looking for the file. GoReleaser writes archives (which are
// all globs) and the checksum file into dist/ and copies nothing else there, so
// a literal in dist/ that is neither the checksum file nor named by a build step
// is a path to nothing.
func TestEveryLiteralAttestationSubjectExists(t *testing.T) {
	root := attestationRepoRoot(t)
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	g := loadGoreleaser(t)

	// The one literal GoReleaser output, derived rather than transcribed.
	generated := map[string]bool{}
	if name := g.Checksum.NameTemplate; name != "" && !strings.ContainsAny(name, "*?[{") {
		generated["dist/"+name] = true
	}

	checked, inRepo := 0, 0
	for job, idxs := range byJob {
		attestIdx := idxs[0]
		// What the job itself BUILDS before the attest step runs.
		built := map[string]bool{}
		for _, p := range distPathsInRunSteps(buildStepsBefore(wf.Jobs[job].Steps, attestIdx)) {
			built[p] = true
		}
		for _, path := range subjectPaths(t, wf.Jobs[job].Steps[attestIdx]) {
			if globMetaPattern.MatchString(path) {
				// A glob cannot be checked here: what it expands to depends on
				// the platform GoReleaser built for. The release job's own guard
				// step covers those, and
				// TestEverySubjectPatternIsCheckedAtReleaseTime keeps that guard
				// in step with this list.
				continue
			}
			checked++
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
				inRepo++
				continue
			}
			if subjectIsJustified(path, generated, built) {
				continue
			}
			t.Errorf("job %q attests the literal path %q, which is neither a file in the repository, a GoReleaser output (%v), nor a path this job builds (%v) — "+
				"a path that does not exist is a glob that matches nothing, and actions/attest drops a zero-match pattern silently when another pattern matched",
				job, path, sortedKeys(generated), sortedKeys(built))
		}
	}
	if checked == 0 {
		t.Fatal("no literal subject path found to check — this test would pass vacuously")
	}
	// Both halves have to be represented, or the rule above is only ever
	// exercised on one kind of literal and the other goes unchecked.
	if inRepo == 0 {
		t.Error("no attested subject path is a repository file; the extra_files half of the rule is untested")
	}
	if len(generated) == 0 {
		t.Error("derived no literal GoReleaser output; the generated half of the rule is untested")
	}
	t.Logf("checked %d literal attestation subject path(s), %d of them repository files", checked, inRepo)
}

// buildStepsBefore returns the steps that run before index attestIdx, minus the
// guard step.
//
// The exclusion is load-bearing and is why this is a function rather than a
// slice. The guard step's entire content is the list of subject paths quoted
// back in order to test them, so counting it as a build step would let the
// guard justify the very paths it exists to check: every literal it names would
// enter the "this job builds it" set and be waved through as a real file while
// nothing builds it. Today the release job's guard names dist/checksums.txt and
// no other step in that job does — and that is exactly why the filter's
// behaviour is tested on synthetic step lists rather than inferred from this
// workflow.
func buildStepsBefore(steps []workflowStep, attestIdx int) []workflowStep {
	var out []workflowStep
	for _, s := range steps[:attestIdx] {
		if s.Name != subjectGuardStepName {
			out = append(out, s)
		}
	}
	return out
}

// TestTheGuardStepIsNotABuildStep keeps the guard out of the build-step set in
// THIS workflow: the guard must be present, must run before the attest step, and
// must not survive the filter. The filter's behaviour beyond that is pinned by
// TestBuildStepsBeforeExcludesTheGuard on synthetic step lists, because anything
// more here would be a statement about this workflow that a later, unrelated
// step could make lapse in silence.
func TestTheGuardStepIsNotABuildStep(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	for job, idxs := range byJob {
		attestIdx := idxs[0]
		if _, err := guardRunsBeforeAttest(wf.Jobs[job].Steps, attestIdx); err != nil {
			t.Errorf("job %q: %v", job, err)
			continue
		}

		kept := buildStepsBefore(wf.Jobs[job].Steps, attestIdx)
		for _, s := range kept {
			if s.Name == subjectGuardStepName {
				t.Errorf("job %q's guard step survived buildStepsBefore — it would justify the paths it only tests", job)
			}
		}

		built := map[string]bool{}
		for _, p := range distPathsInRunSteps(kept) {
			built[p] = true
		}
		// Deliberately nothing more. An earlier version of this test also
		// asserted that filtering the guard changes the answer for some job,
		// which reads like coverage of the filter but is really a statement
		// about THIS workflow: it held only because the release job's guard
		// names dist/checksums.txt and no other step in that job does. Adding a
		// step that names it — a `sha256sum -c dist/checksums.txt`, say — would
		// make the assertion lapse in silence, taking the coverage with it. The
		// filter's behaviour is pinned by TestBuildStepsBeforeExcludesTheGuard
		// on synthetic step lists instead, where it cannot lapse.
		t.Logf("job %q: %d dist/ path(s) named by its build steps", job, len(built))
	}
}

// publishedReleaseGuardStepName is the step that makes the draft assumption
// above an enforced one rather than a claim.
const publishedReleaseGuardStepName = "Refuse to build into a published release"

// TestTheReleaseJobRefusesAPublishedRelease is the other half of
// TestTheReleaseJobIsReRunnable, and it exists because replace_existing_artifacts
// is only safe while the release is a DRAFT.
//
// The plugin job's failure-ordering comment documents a window — "after publish,
// before catalog push" — in which a re-run is safe, and a "Re-run all jobs" in
// that window re-runs the RELEASE job too, with GoReleaser pointed at a release
// that is now public and holds every asset. Without a guard, that re-run hits
// every 422 and, because this change sets replace_existing_artifacts, responds
// by DELETING each colliding asset and re-uploading it. So the failure mode
// moves from a loud 422 — the whole point of keeping the plugin job's ordering
// documented — to a silent replacement of published, already-attested assets
// under a catalog that main still pins to the old digests.
//
// The guard makes the loud case loud again, with a message that says what to do,
// and it is asserted to run BEFORE GoReleaser because after the upload the
// damage is done.
func TestTheReleaseJobRefusesAPublishedRelease(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	steps := wf.Jobs["release"].Steps

	guardIdx, goreleaserIdx := -1, -1
	for i, s := range steps {
		switch {
		case s.Name == publishedReleaseGuardStepName:
			guardIdx = i
		case strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@"):
			goreleaserIdx = i
		}
	}
	if goreleaserIdx == -1 {
		t.Fatal("no goreleaser step found in the release job")
	}
	if guardIdx == -1 {
		t.Errorf("the release job has no %q step, so a re-run in the plugin job's documented \"after publish, before catalog push\" window would run GoReleaser against a PUBLISHED release and replace_existing_artifacts would delete and re-upload its attested assets",
			publishedReleaseGuardStepName)
		return
	}
	if guardIdx > goreleaserIdx {
		t.Errorf("%q runs at %d, after GoReleaser at %d; the assets are uploaded by then and the replacement has already happened",
			publishedReleaseGuardStepName, guardIdx, goreleaserIdx)
	}
	// And it has to be able to see the release, which means the API scope the
	// check needs is one the job already holds.
	if got := wf.Jobs["release"].Permissions["contents"]; got != "write" {
		t.Errorf("the release job holds contents: %q, so the guard cannot read the release for the tag", got)
	}
}

// TestTheReleaseJobIsReRunnable keeps the documented recovery real.
//
// This change puts two steps in the release job AFTER GoReleaser has uploaded
// every asset to the draft: the zero-match subject guard, and the attest step,
// which depends on Sigstore, Fulcio and GitHub's OIDC provider. Any of those
// can fail on a transient fault, and the recovery the workflow comments
// describe — and the only one a maintainer has — is to re-run the job.
//
// A re-run starts at the first step, so it runs GoReleaser again, and
// use_existing_draft makes GoReleaser ADOPT the draft that already holds every
// uploaded asset. Each archive then collides with its own name, GitHub answers
// 422, and GoReleaser's Upload (internal/client/github.go) returns:
//
//	if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity {
//	    if !ctx.Config.Release.ReplaceExistingArtifacts {
//	        return err
//	    }
//	    if err := c.deleteReleaseArtifact(ctx, githubReleaseID, artifact.Name, 1); err != nil { return err }
//	    return RetriableError{err}
//	}
//
// That first return is a plain error, not a RetriableError: the run aborts on
// the first archive, and the draft is stuck holding assets that can only be
// cleared by hand. So a transient Sigstore outage would have turned the release
// job into a one-shot.
//
// replace_existing_artifacts makes the re-run delete the colliding asset and
// retry it, which is the same policy the plugin job's own `gh release upload
// --clobber` already applies to the archives it attaches.
//
// It is safe HERE because of draft: true, and the test asserts the two
// together rather than the setting alone. GoReleaser's PublishRelease is a
// no-op while release.draft is set, so this job never publishes — the plugin
// job does, with `gh release edit --draft=false`, and only after the
// attestations are in place. The assets replace_existing_artifacts overwrites
// are therefore always unpublished ones. If draft were ever turned off, the
// same setting would start overwriting assets on a PUBLIC release, so that is
// the pair this test holds together.
//
// "Unpublished" is the part that needs a second guard rather than just a
// claim, and that guard is the release job refusing to run against a published
// release: the plugin job's failure-ordering comment blesses a re-run in the
// window after the publish, and a "Re-run all jobs" there re-runs THIS job
// with GoReleaser pointed at a public release. TestTheReleaseJobRefusesAPublished
// Release is that guard, and the pair of tests is the whole argument.
func TestTheReleaseJobIsReRunnable(t *testing.T) {
	g := loadGoreleaser(t)

	if !g.Release.Draft {
		t.Error("release.draft is false, so this job publishes its own release — which would make replace_existing_artifacts overwrite assets on a PUBLIC release, and makes the draft-then-publish recovery the plugin job depends on impossible")
	}
	if !g.Release.UseExistingDraft {
		t.Error("release.use_existing_draft is false, so a re-run creates a SECOND release for the same tag instead of completing the first — a duplicate-draft release that nothing cleans up")
	}
	if !g.Release.ReplaceExistingArtifacts {
		t.Errorf("release.replace_existing_artifacts is false, so a re-run after any post-upload failure (the %q guard, the attest step) dies in GoReleaser on the first already-uploaded archive: GitHub answers 422 and internal/client/github.go returns a plain error rather than deleting the asset and retrying. "+
			"The release job is then stuck holding a draft nobody can complete without deleting its assets by hand",
			subjectGuardStepName)
	}
}

// TestAttestationDoesNotPushToTheRegistry keeps the two attestation scopes the
// whole of what the attest step needs. The concern is real and the resolution
// is NOT to add `packages: write`:
//
//	actions/attest-build-provenance at the pinned SHA
//	(4d101475d8b20a2381f78447822ac1eab6504dd8, v4.2.2) declares
//	  push-to-registry:
//	    default: false
//	    description: "…Requires that the \"subject-name\" parameter specify the
//	                  fully-qualified image name and that the \"subject-digest\"
//	                  parameter be specified. Defaults to false."
//
// So the registry push is off, and it could not run in any case: it requires
// subject-name and subject-digest, and both attest steps use subject-path.
//
// This test is what keeps that true rather than assumed. It is not a check on
// the upstream action — a Go test cannot read action.yml — but it is a check on
// the combination that would break a release: enabling the registry push
// without the scope it needs. Whoever bumps the action's SHA re-reads the
// action.yml above, because this test's comment is where the evidence lives.
func TestAttestationDoesNotPushToTheRegistry(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	if len(byJob) == 0 {
		t.Fatal("no job invokes " + attestAction)
	}

	for job, idxs := range byJob {
		step := wf.Jobs[job].Steps[idxs[0]]
		if got, ok := step.With["push-to-registry"]; ok && got != "false" {
			t.Errorf("job %q sets push-to-registry: %q, which pushes the attestation into ghcr.io and needs packages: write that this job deliberately does not hold", job, got)
		}
		if got := wf.Jobs[job].Permissions["packages"]; got != "" {
			t.Errorf("job %q holds packages: %s; nothing in the release path pushes an image, and the attest step does not push to the registry", job, got)
		}
	}
}

// guardRunsBeforeAttest locates the guard step in the WHOLE job and reports
// where it is relative to the attest step.
//
// It searches the whole job rather than the prefix before the attest step, and
// that is the load-bearing part: a guard placed AFTER the attest step is the
// case this exists to catch, and a prefix search cannot see it — the index
// comparison would be comparing a bound against itself and would pass.
func guardRunsBeforeAttest(steps []workflowStep, attestIdx int) (guardIdx int, err error) {
	guardIdx = -1
	for i, s := range steps {
		if s.Name != subjectGuardStepName {
			continue
		}
		if guardIdx != -1 {
			return -1, fmt.Errorf("two %q steps; the one that runs is ambiguous", subjectGuardStepName)
		}
		guardIdx = i
	}
	switch {
	case guardIdx == -1:
		return -1, fmt.Errorf("no %q step, so nothing checks a zero-match pattern", subjectGuardStepName)
	case guardIdx >= attestIdx:
		return -1, fmt.Errorf("the guard runs at %d, at or after the attest step at %d; the check would be too late to stop anything", guardIdx, attestIdx)
	}
	return guardIdx, nil
}

// TestBuildStepsBeforeExcludesTheGuard pins the filter's behaviour on step
// lists built for the purpose, rather than on the release workflow.
//
// This is the check that was moved out of TestTheGuardStepIsNotABuildStep
// because there it was a statement about THIS workflow: it held only while the
// release job's guard was the sole step naming dist/checksums.txt, and a single
// new step that names the file would have made it lapse in silence. Here the
// fixtures say what they mean, so the filter is load-bearing by construction.
func TestBuildStepsBeforeExcludesTheGuard(t *testing.T) {
	build := workflowStep{Name: "Build", Run: "unzip -l dist/ghost-plugin.zip | head -20"}
	// A guard that quotes a path no build step names — the shape that made the
	// existence check justify itself.
	guard := workflowStep{Name: subjectGuardStepName,
		Run: "for pattern in \"dist/checksums.txt\" \"dist/*.zip\"; do :; done"}
	attest := workflowStep{Name: "Attest", Uses: attestAction + "@" + strings.Repeat("a", 40)}

	kept := buildStepsBefore([]workflowStep{build, guard, attest}, 2)
	for _, s := range kept {
		if s.Name == subjectGuardStepName {
			t.Fatal("the guard step survived buildStepsBefore")
		}
	}
	got := distPathsInRunSteps(kept)
	if len(got) != 1 || got[0] != "dist/ghost-plugin.zip" {
		t.Errorf("build steps name %v, want only the archive the build step writes — the guard's own quotes must not appear", got)
	}

	// And the converse: a path a build step genuinely writes is kept, so the
	// filter is not simply dropping everything.
	builds := workflowStep{Name: "Build", Run: "sha256sum -c dist/checksums.txt"}
	got = distPathsInRunSteps(buildStepsBefore([]workflowStep{builds, guard, attest}, 2))
	if len(got) != 1 || got[0] != "dist/checksums.txt" {
		t.Errorf("build steps name %v, want the manifest a build step really checks", got)
	}

	// Renaming the guard must not be able to smuggle it back in as a build step
	// for as long as the workflow and the test agree on the name; the workflow's
	// guard step is located BY that name, so a rename is a build failure.
	if _, err := guardRunsBeforeAttest([]workflowStep{build, attest}, 1); err == nil {
		t.Error("a job with no guard was accepted")
	}
}

// TestGuardRunsBeforeAttest exercises the ordering rule on step lists where it
// can fail, which the real workflow cannot supply — its guard is already in the
// right place, so against it the comparison is a tautology and every version of
// it passes.
func TestGuardRunsBeforeAttest(t *testing.T) {
	attest := workflowStep{Name: "Attest the release archives and checksums",
		Uses: attestAction + "@" + strings.Repeat("a", 40)}
	guard := workflowStep{Name: subjectGuardStepName, Run: "true"}
	other := workflowStep{Name: "Build", Run: "true"}

	for _, tc := range []struct {
		name      string
		steps     []workflowStep
		attestIdx int
		wantErr   string
	}{
		{"guard before the attest step", []workflowStep{other, guard, attest}, 2, ""},
		{"guard after the attest step", []workflowStep{other, attest, guard}, 1,
			"at or after the attest step"},
		{"guard immediately after the attest step", []workflowStep{attest, guard}, 0,
			"at or after the attest step"},
		{"no guard at all", []workflowStep{other, attest}, 1, "nothing checks a zero-match pattern"},
		{"two guards", []workflowStep{guard, guard, attest}, 2, "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, err := guardRunsBeforeAttest(tc.steps, tc.attestIdx)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if idx >= tc.attestIdx {
					t.Errorf("guard index %d is not before the attest step at %d", idx, tc.attestIdx)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a guard that is %s (index %d, attest at %d)", tc.wantErr, idx, tc.attestIdx)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// subjectIsJustified is the rule TestEveryLiteralAttestationSubjectExists
// applies to a literal subject path that is not a file in the repository: it
// must be a path some build step is known to produce.
//
// It is a named function with a test of its own, because a rule that is only
// ever exercised against a correct workflow cannot be told apart from a rule
// that is too weak. A weaker version of this — "anything under dist/ is a build
// output" — agrees with the real one on every path the release happens to use
// today, and disagrees on exactly the path that caused the blocker.
func subjectIsJustified(path string, generated, built map[string]bool) bool {
	return generated[path] || built[path]
}

// TestSubjectIsJustified is that self-check: the rule must reject the path that
// shipped unattested, and accept the two kinds that are real. A rule that
// accepts `dist/install.ps1` because it is dist/-prefixed would pass every
// other test in this file.
func TestSubjectIsJustified(t *testing.T) {
	generated := map[string]bool{"dist/checksums.txt": true}
	built := map[string]bool{"dist/ghost-plugin.zip": true}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"the checksum file GoReleaser writes", "dist/checksums.txt", true},
		{"an archive the job builds", "dist/ghost-plugin.zip", true},
		// The blocker: an extra file with a dist/ prefix. GoReleaser never
		// copies an extra file into dist/, so this names nothing, and it is
		// dist/-prefixed, so a prefix-based rule would wave it through.
		{"the extra file with a dist/ prefix", "dist/install.ps1", false},
		{"an extra file at its real path", "install.ps1", false},
		{"an archive glob", "dist/*.tar.gz", false},
		{"a plausible typo", "dist/checksum.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subjectIsJustified(tc.path, generated, built); got != tc.want {
				t.Errorf("subjectIsJustified(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestSubjectGuardPatternsFindsNothingWithoutTheStep is the other self-check.
// Reading the guard's list out of a step that is not there has to yield
// nothing, so that a job with no guard is a failure rather than a comparison
// that quietly compares the attest step against itself.
func TestSubjectGuardPatternsFindsNothingWithoutTheStep(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	// The real plugin job, with its guard step removed.
	steps := make([]workflowStep, 0, len(wf.Jobs["plugin"].Steps))
	for _, s := range wf.Jobs["plugin"].Steps {
		if s.Name == subjectGuardStepName {
			continue
		}
		steps = append(steps, s)
	}
	if got := subjectGuardPatterns(t, steps, "plugin"); got != nil {
		t.Errorf("with the guard step deleted, the reader returned %v, want nil — a job with no guard must be a failure, not a tautology", got)
	}
	// And the real job, guard intact, must yield its full list.
	full := subjectGuardPatterns(t, wf.Jobs["plugin"].Steps, "plugin")
	want := subjectPaths(t, wf.Jobs["plugin"].Steps[byJob["plugin"][0]])
	equalStringSets(t, "the plugin job's guard step lists every attested pattern", full, want)
}

// TestTheReleaseTimeGuardFailsOnASingleZeroMatchPattern executes the guard
// steps' own shell. Every other test here reads the guard's shape; none of them
// can tell a guard that fails from a guard that walks away quietly, and a guard
// that never fails is exactly as silent as the bug it replaced. So the scripts
// are run, under the same `bash -e` the runner uses, against a directory built
// to match — and then against one that does not.
//
// The failure case removes a file that only ONE job's patterns need, so each
// job's guard is shown to be load-bearing on its own: the release job's
// `dist/*.zip` still matches without the missing plugin archive.
func TestTheReleaseTimeGuardFailsOnASingleZeroMatchPattern(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH; the guard steps are bash")
	}
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	// Every literal path any attesting job declares, plus one file for each
	// glob's extension, so "everything matches" is the starting state. A literal
	// belongs to exactly one job's list, which is what makes the failure case
	// below attributable.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	allLiterals := []string{}
	byJobLiterals := map[string][]string{}
	for job, idxs := range byJob {
		for _, path := range subjectPaths(t, wf.Jobs[job].Steps[idxs[0]]) {
			switch {
			case path == "dist/*.tar.gz":
				writeFixture(t, dir, "dist/ghost_0.0.0_linux_amd64.tar.gz")
			case path == "dist/*.zip":
				writeFixture(t, dir, "dist/ghost_0.0.0_linux_amd64.zip")
			case globMetaPattern.MatchString(path):
				// A glob whose every expansion another job also needs cannot
				// isolate a job, so it is left to the happy path.
			default:
				writeFixture(t, dir, path)
				allLiterals = append(allLiterals, path)
				byJobLiterals[job] = append(byJobLiterals[job], path)
			}
		}
	}

	for job := range byJob {
		t.Run(job+" with every subject present", func(t *testing.T) {
			script := guardScript(t, wf.Jobs[job].Steps)
			if code, out := runBashScript(t, dir, script); code != 0 {
				t.Errorf("the guard failed with every subject present (exit %d):\n%s", code, out)
			}
		})
		t.Run(job+" with one of its own subjects missing", func(t *testing.T) {
			victims := byJobLiterals[job]
			if len(victims) == 0 {
				t.Skipf("job %q declares no literal subject, so its failure cannot be isolated from the other job's", job)
			}
			victim := victims[len(victims)-1]
			// A tree with every literal except the victim, so the OTHER job's
			// guard still passes and this job's does not.
			bare := t.TempDir()
			if err := os.MkdirAll(filepath.Join(bare, "dist"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, path := range allLiterals {
				if path != victim {
					writeFixture(t, bare, path)
				}
			}
			writeFixture(t, bare, "dist/ghost_0.0.0_linux_amd64.tar.gz")
			writeFixture(t, bare, "dist/ghost_0.0.0_linux_amd64.zip")
			script := guardScript(t, wf.Jobs[job].Steps)
			code, out := runBashScript(t, bare, script)
			if code == 0 {
				t.Errorf("the guard exited 0 with %s missing, so the release would publish it with no attestation:\n%s", victim, out)
			}
			if !strings.Contains(out, victim) {
				t.Errorf("the failure does not name the missing subject %q:\n%s", victim, out)
			}
		})
	}
}

func writeFixture(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// guardScript returns the guard step's run body, failing the test if the job has
// no guard step.
func guardScript(t *testing.T, steps []workflowStep) string {
	t.Helper()
	for _, s := range steps {
		if s.Name == subjectGuardStepName {
			if s.Run == "" {
				t.Fatalf("step %q has no run body", subjectGuardStepName)
			}
			return s.Run
		}
	}
	t.Fatalf("no step named %q", subjectGuardStepName)
	return ""
}

// runBashScript runs script with dir as the working directory, under the
// `bash -e {0}` the Actions runner uses, and returns its exit code and output.
func runBashScript(t *testing.T, dir, script string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", "-e", "-c", script)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, buf.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), buf.String()
	default:
		t.Fatalf("running the guard script: %v", err)
		return 0, ""
	}
}

// distPathsInRunSteps collects the dist/-prefixed paths a run step names,
// flattening line continuations and dropping comments first. It is deliberately
// a word scan rather than a shell parse: the claim being tested is "a step in
// this job names this file", and a word is the least that can establish it.
func distPathsInRunSteps(steps []workflowStep) []string {
	var out []string
	for _, step := range steps {
		if step.Run == "" {
			continue
		}
		var kept []string
		for _, line := range strings.Split(step.Run, "\n") {
			if idx := strings.Index(line, "#"); idx >= 0 {
				line = line[:idx]
			}
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				kept = append(kept, trimmed)
			}
		}
		for _, field := range strings.Fields(strings.Join(kept, " ")) {
			clean := strings.Trim(field, "\"'")
			if !strings.HasPrefix(clean, "dist/") || globMetaPattern.MatchString(clean) {
				continue
			}
			out = append(out, clean)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestEverySubjectPatternIsCheckedAtReleaseTime is the runtime half of the same
// hole. actions/attest-build-provenance only fails when the COMBINED glob
// result is empty (src/subject.ts, getSubjectFromPath), so one pattern matching
// nothing is dropped with no error at all, and the release ships an asset
// nobody attested.
//
// A Go test cannot observe that: the expansion happens on a runner with a
// dist/ this repository has never seen. So the release job carries its own
// guard that fails on a single zero-match pattern, and this test is what keeps
// the guard's list identical to the attest step's list — two hand-written lists
// that agreed by eye is precisely how `dist/install.ps1` shipped.
func TestEverySubjectPatternIsCheckedAtReleaseTime(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

	for job, idxs := range byJob {
		attested := subjectPaths(t, wf.Jobs[job].Steps[idxs[0]])

		guards := subjectGuardPatterns(t, wf.Jobs[job].Steps, job)
		if len(guards) == 0 {
			t.Errorf("job %q attests %d subject path(s) but carries no step that checks them at release time; "+
				"actions/attest drops a pattern that matches nothing when another matched, so an unattested asset would ship silently",
				job, len(attested))
			continue
		}
		equalStringSets(t, "job "+job+": the release-time guard's patterns vs the attest step's subject-path",
			guards, attested)
	}
}

// subjectGuardStepName is the step each attesting job carries to fail on an
// attestation subject pattern that matches nothing. It is matched by name
// rather than by "some run step that mentions the patterns", because the
// plugin job's `gh release upload` step already names all three of its zips and
// would pass such a test without checking anything.
const subjectGuardStepName = "Every attestation subject exists"

// subjectGuardPatterns returns the patterns the job's guard step checks.
func subjectGuardPatterns(t *testing.T, steps []workflowStep, job string) []string {
	t.Helper()
	attested := map[string]bool{}
	for _, step := range steps {
		if !strings.HasPrefix(step.Uses, attestAction+"@") {
			continue
		}
		for _, p := range subjectPaths(t, step) {
			attested[p] = true
		}
	}
	if len(attested) == 0 {
		t.Fatalf("job %q has no attest step", job)
	}

	for _, step := range steps {
		if step.Name != subjectGuardStepName {
			continue
		}
		if step.Run == "" {
			t.Fatalf("job %q has a step named %q with no run body", job, subjectGuardStepName)
		}
		var found []string
		// Drop comments, join continuations, then take the words that name a
		// declared subject. A word is the least that can establish "this step
		// checks that pattern", and the equality below is what makes the two
		// lists unable to drift.
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
		for _, word := range fields {
			word = strings.Trim(word, "\"'")
			if attested[word] {
				found = append(found, word)
			}
		}
		return found
	}
	return nil
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
