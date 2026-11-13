package selfupdate

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The producer side of the release-authenticity contract (#694). The client

const attestAction = "actions/attest-build-provenance"

type workflowStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

type workflowJob struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type releaseWorkflow struct {
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

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
		Draft                    bool `yaml:"draft"`
		UseExistingDraft         bool `yaml:"use_existing_draft"`
		ReplaceExistingArtifacts bool `yaml:"replace_existing_artifacts"`
	} `yaml:"release"`
}

var shaPinPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(attestAction) + `@[0-9a-f]{40}$`)

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

// will trust, so a floating tag on it is the exact hole #694 is about. The
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

var globMetaPattern = regexp.MustCompile(`[*?\[]`)

func subjectIsJustified(path string, generated, built map[string]bool) bool {
	return generated[path] || built[path]
}

func TestEveryLiteralAttestationSubjectExists(t *testing.T) {
	root := attestationRepoRoot(t)
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)
	g := loadGoreleaser(t)

	generated := map[string]bool{}
	if name := g.Checksum.NameTemplate; name != "" && !strings.ContainsAny(name, "*?[{") {
		generated["dist/"+name] = true
	}

	checked, inRepo := 0, 0
	for job, idxs := range byJob {
		attestIdx := idxs[0]
		built := map[string]bool{}
		for _, p := range distPathsInRunSteps(buildStepsBefore(wf.Jobs[job].Steps, attestIdx)) {
			built[p] = true
		}
		for _, path := range subjectPaths(t, wf.Jobs[job].Steps[attestIdx]) {
			if globMetaPattern.MatchString(path) {
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
	if inRepo == 0 {
		t.Error("no attested subject path is a repository file; the extra_files half of the rule is untested")
	}
	if len(generated) == 0 {
		t.Error("derived no literal GoReleaser output; the generated half of the rule is untested")
	}
	t.Logf("checked %d literal attestation subject path(s), %d of them repository files", checked, inRepo)
}

func buildStepsBefore(steps []workflowStep, attestIdx int) []workflowStep {
	var out []workflowStep
	for _, s := range steps[:attestIdx] {
		if s.Name != subjectGuardStepName {
			out = append(out, s)
		}
	}
	return out
}

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
		t.Logf("job %q: %d dist/ path(s) named by its build steps", job, len(built))
	}
}

func requireBash4(t *testing.T) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH; the guard steps are bash")
	}
	out, err := exec.Command(bash, "-c", `echo "${BASH_VERSINFO[0]:-0}"`).Output()
	if err != nil {
		t.Skipf("cannot determine the bash version: %v", err)
	}
	major, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		t.Skipf("cannot read the bash major version from %q", out)
	}
	if major < 4 {
		t.Skipf("bash %d is too old: the guard steps use mapfile, which is bash 4.0+", major)
	}
}

func TestTheReleaseTimeGuardFailsOnASingleZeroMatchPattern(t *testing.T) {
	requireBash4(t)
	wf, _ := loadReleaseWorkflow(t)
	byJob := attestStepsByJob(wf)

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
			if code, out := runBashScript(t, dir, script, nil); code != 0 {
				t.Errorf("the guard failed with every subject present (exit %d):\n%s", code, out)
			}
		})
		t.Run(job+" with one of its own subjects missing", func(t *testing.T) {
			victims := byJobLiterals[job]
			if len(victims) == 0 {
				t.Skipf("job %q declares no literal subject, so its failure cannot be isolated from the other job's", job)
			}
			victim := victims[len(victims)-1]
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
			code, out := runBashScript(t, bare, script, nil)
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

func runBashScript(t *testing.T, dir, script string, env map[string]string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", "-e", "-c", script)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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

const subjectGuardStepName = "Every attestation subject exists"

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

func templateToGlob(template string) string {
	replaced := regexp.MustCompile(`\{\{[^}]*\}\}`).ReplaceAllString(template, "*")
	return replaced
}

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

func uploadedPaths(t *testing.T, steps []workflowStep, command ...string) []string {
	t.Helper()
	words := len(command)
	var out []string
	for _, step := range steps {
		if !strings.Contains(step.Run, strings.Join(command, " ")) {
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

// by the same name the workflow uses. The PR step is the one #888 is about:
const (
	catalogPRStepName     = "Open or update the catalog pull request"
	manualMergeStepName   = "Report the manual merge step"
	verifyCatalogStepName = "Verify the catalog on the pin branch matches the pinned copy"
)

const pinBranch = "automation/marketplace-pin-v0.45.0"

const refusedPRCreate = "pull request create failed: GraphQL: GitHub Actions is not permitted to create or approve pull requests (createPullRequest)"

func findPluginStep(t *testing.T, wf releaseWorkflow, name string) workflowStep {
	t.Helper()
	for _, s := range wf.Jobs["plugin"].Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("the plugin job has no step named %q", name)
	return workflowStep{}
}

func TestPRStepHasExpectedID(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	step := findPluginStep(t, wf, catalogPRStepName)
	if step.ID != "open-pr" {
		t.Errorf("the PR step's id is %q, want \"open-pr\" — the summary step's outputs depend on this", step.ID)
	}
}

type pluginStepSandbox struct {
	dir     string
	bin     string
	scratch string
}

func newPluginStepSandbox(t *testing.T) *pluginStepSandbox {
	t.Helper()
	dir := t.TempDir()
	writeCatalogFixture(t, dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStubGH(t, bin)
	scratch := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	return &pluginStepSandbox{dir: dir, bin: bin, scratch: scratch}
}

func (s *pluginStepSandbox) env(extra map[string]string) map[string]string {
	env := map[string]string{
		"PATH":              s.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_REPOSITORY": "wcatz/ghost",
		"GITHUB_REF_NAME":   "v0.45.0",
		"GH_TOKEN":          "stub-token",
		"STUB_LOG":          filepath.Join(s.scratch, "gh-calls.log"),
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func requireShellTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "base64", "cmp", "diff"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; the plugin job's steps are shell that shells out to it. "+
				"Skipping rather than failing: a step that cannot run is a gap to close, not a defect to report", tool)
		}
	}
}

func (s *pluginStepSandbox) runStep(t *testing.T, step workflowStep, outputs, env map[string]string) (int, string) {
	t.Helper()
	requireShellTools(t)
	script := expandStepOutputs(t, withOwnScratch(step.Run, s.scratch), outputs)
	return runBashScript(t, s.dir, script, env)
}

func writeCatalogFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "plugins": [
    {
      "name": "ghost",
      "source": {
        "type": "github",
        "url": "https://github.com/wcatz/ghost/releases/download/v0.45.0/ghost-plugin.zip",
        "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      }
    }
  ]
}
`
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #888 is about, the successful create, and the contents-API read the
func writeStubGH(t *testing.T, bin string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -u
{
  printf 'gh'
  for arg in "$@"; do printf ' %s' "$arg"; done
  printf '\n'
} >> "$STUB_LOG"
case "$1" in
  pr)
    case "$2" in
      list)
        printf '%s' "${STUB_PR_LIST:-}"
        exit "${STUB_PR_LIST_EXIT:-0}"
        ;;
      create)
        printf '%s' "${STUB_PR_CREATE_STDOUT:-}"
        printf '%s' "${STUB_PR_CREATE_STDERR:-}" >&2
        exit "${STUB_PR_CREATE_EXIT:-0}"
        ;;
      view)
        # gh pr view takes ONE --json field, and the step asks for two
        # different things: the URL the summary reports, and the author that
        # decides whether GitHub holds the PR pending approval. Answer the
        # author query when it is the one being asked.
        for arg in "$@"; do
          if [ "$arg" = "author" ]; then
            # Author query can fail independently of URL query — use a
            # separate exit var so tests can express "URL resolved, author
            # failed".
            if [ -n "${STUB_PR_AUTHOR_EXIT:-}" ] && [ "${STUB_PR_AUTHOR_EXIT}" != "0" ]; then
              printf '%s' "${STUB_PR_AUTHOR_STDERR:-}" >&2
              exit "${STUB_PR_AUTHOR_EXIT}"
            fi
            printf '%s' "${STUB_PR_AUTHOR:-}"
            exit "${STUB_PR_VIEW_EXIT:-0}"
          fi
        done
        printf '%s' "${STUB_PR_VIEW_URL:-}"
        exit "${STUB_PR_VIEW_EXIT:-0}"
        ;;
      edit)
        exit "${STUB_PR_EDIT_EXIT:-0}"
        ;;
      *)
        printf 'stub gh: unhandled pr subcommand: %s\n' "$2" >&2
        exit 64
        ;;
    esac
    ;;
  api)
    if [ "${STUB_API_EXIT:-0}" != "0" ]; then
      printf '%s' "${STUB_API_STDERR:-}" >&2
      exit "$STUB_API_EXIT"
    fi
    # A caller can answer with a raw JSON body instead of a content field, to
    # exercise a response the workflow must reject rather than decode.
    if [ -n "${STUB_CONTENTS_RAW:-}" ]; then
      printf '%s' "$STUB_CONTENTS_RAW"
      exit 0
    fi
    if [ -z "${STUB_CONTENTS_B64:-}" ]; then
      printf 'stub gh: api called with no STUB_CONTENTS_B64\n' >&2
      exit 64
    fi
    printf '{"content": "%s"}' "$STUB_CONTENTS_B64"
    exit 0
    ;;
  *)
    printf 'stub gh: unhandled command: %s\n' "$1" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func withOwnScratch(script, scratch string) string {
	return strings.ReplaceAll(script, "/tmp/", scratch+"/")
}

var stepOutputExpr = regexp.MustCompile(`\$\{\{\s*steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)\s*\}\}`)

func expandStepOutputs(t *testing.T, script string, outputs map[string]string) string {
	t.Helper()
	out := stepOutputExpr.ReplaceAllStringFunc(script, func(m string) string {
		sub := stepOutputExpr.FindStringSubmatch(m)
		key := sub[1] + "." + sub[2]
		return outputs[key]
	})
	if strings.Contains(out, "${{") {
		t.Fatalf("script contains an expression this test does not model:\n%s", out)
	}
	return out
}

func readStepOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read GITHUB_OUTPUT %s: %v", path, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed GITHUB_OUTPUT line %q", line)
		}
		out[k] = v
	}
	return out
}

func runCatalogPRStep(t *testing.T, step workflowStep, stubEnv map[string]string) (int, string, map[string]string) {
	t.Helper()
	sb := newPluginStepSandbox(t)
	output := filepath.Join(sb.scratch, "github-output")
	env := sb.env(stubEnv)
	env["GITHUB_OUTPUT"] = output
	code, out := sb.runStep(t, step, map[string]string{"pin.BRANCH": pinBranch}, env)
	return code, out, readStepOutputs(t, output)
}

// TestCatalogPRStepToleratesARefusedPullRequest is the behaviour #888 is
func TestCatalogPRStepToleratesARefusedPullRequest(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	step := findPluginStep(t, wf, catalogPRStepName)
	if step.Run == "" {
		t.Fatalf("step %q has no run body", catalogPRStepName)
	}

	code, out, outputs := runCatalogPRStep(t, step, map[string]string{
		"STUB_PR_CREATE_EXIT":   "1",
		"STUB_PR_CREATE_STDERR": refusedPRCreate,
	})
	if code != 0 {
		t.Errorf("the PR step exited %d on a refused `gh pr create`, so the release job ends red and the two steps after it are skipped:\n%s", code, out)
	}
	wantCompare := "https://github.com/wcatz/ghost/compare/main..." + pinBranch + "?expand=1"
	if got := outputs["COMPARE_URL"]; got != wantCompare {
		t.Errorf("COMPARE_URL = %q, want %q — the summary's manual step is this link", got, wantCompare)
	}
	if got := outputs["PR_OPENED"]; got != "no" {
		t.Errorf("PR_OPENED = %q, want \"no\" — the step must say whether a PR was opened", got)
	}
	if got := outputs["PR_HELD_FOR_APPROVAL"]; got != "no" {
		t.Errorf("PR_HELD_FOR_APPROVAL = %q, want \"no\" — no PR exists, so nothing is held", got)
	}
	if _, ok := outputs["PR_URL"]; ok {
		t.Errorf("PR_URL was written even though no PR was opened: %q", outputs["PR_URL"])
	}
	if !strings.Contains(out, wantCompare) {
		t.Errorf("the step's log does not surface the compare link a human needs:\n%s", out)
	}
}

func TestTheCatalogVerificationRunsAfterARefusedPullRequest(t *testing.T) {
	wf, _ := loadReleaseWorkflow(t)
	prStep := findPluginStep(t, wf, catalogPRStepName)
	verify := findPluginStep(t, wf, verifyCatalogStepName)

	if strings.Contains(verify.If, "open-pr") {
		t.Errorf("the verification step's `if` is %q — gating it on the PR step would skip it exactly when the PR is refused, which is the bug #888 had", verify.If)
	}
	if !strings.Contains(verify.If, "steps.pin.outputs.SKIPPED") {
		t.Errorf("the verification step's `if` is %q — it must still gate on the pin step's skip flag", verify.If)
	}

	code, out, _ := runCatalogPRStep(t, prStep, map[string]string{
		"STUB_PR_CREATE_EXIT":   "1",
		"STUB_PR_CREATE_STDERR": refusedPRCreate,
	})
	if code != 0 {
		t.Fatalf("the PR step exited %d on a refusal, so the runner would skip the verification step:\n%s", code, out)
	}

	sb := newPluginStepSandbox(t)
	catalog := filepath.Join(sb.dir, ".claude-plugin", "marketplace.json")
	raw, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	code, out = sb.runStep(t, verify, map[string]string{"pin.BRANCH": pinBranch}, sb.env(map[string]string{
		"STUB_CONTENTS_B64": base64.StdEncoding.EncodeToString(raw),
	}))
	if code != 0 {
		t.Errorf("the verification step exited %d after a refused PR:\n%s", code, out)
	}
}
