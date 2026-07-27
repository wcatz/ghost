export const meta = {
  name: 'ghost-eval',
  description: 'Real-world eval of Ghost memory quality: live save-decision replay, reflect consolidation, storyline injection/search, stress scenarios, synthesis report',
  whenToUse: 'Run on demand to diagnose Ghost memory quality against real usage. Not wired into CI.',
  phases: [
    { title: 'Setup' },
    { title: 'Replay' },
    { title: 'Consolidation' },
    { title: 'Storyline' },
    { title: 'Stress' },
    { title: 'Synthesize' },
  ],
}

const REPO = args && args.repoPath ? args.repoPath : '/home/wayne/git/ghost'
const REAL_DB = '/home/wayne/.local/share/ghost/ghost.db'

phase('Setup')
const runId = await agent(
  'Run exactly this command and return ONLY its stdout, nothing else: date +%Y%m%d-%H%M%S-eval',
  { label: 'run-id' }
)
const scratchRoot = `/tmp/ghost-eval/${runId.trim()}`
log(`Eval run ${runId.trim()} — scratch root ${scratchRoot}`)

await agent(
  `Run: mkdir -p ${scratchRoot}/data ${scratchRoot}/config && echo ready`,
  { label: 'scratch-mkdir' }
)

// Later phases each mint their own unit run-id as `${runId.trim()}-<unit-key>`
// (unit-key = replay project id, storyline key, or stress scenario key) and,
// inside the agent prompt that does the actual live-agent work, shell out to:
//   bash ${REPO}/docs/superpowers/eval/lib/make-unit-config.sh <unit-run-id> ${REPO}/docs/superpowers/eval/lib/ghost-wrapped ${REPO}/ghost
//   bash ${REPO}/docs/superpowers/eval/lib/claude-eval-session.sh <unit-run-id> "<prompt>"
// A storyline's sessions all reuse the SAME unit-run-id (config built once,
// before session 1) so session N+1's scratch DB actually contains what
// session N saved. Every other unit type gets its own unit-run-id.

// ... phases added in later tasks ...

phase('Synthesize')
// placeholder until Task 9 fills this in
const report = { note: 'synthesis not yet implemented — see plan Task 9' }

await agent(
  `Run: rm -rf ${scratchRoot} && echo cleaned`,
  { label: 'cleanup' }
)

return report
