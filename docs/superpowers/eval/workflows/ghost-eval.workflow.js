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

// args sometimes arrives as a JSON-encoded string rather than a parsed
// object (observed with the Workflow tool's args passthrough) — normalize.
const parsedArgs = typeof args === 'string' ? JSON.parse(args) : args

const REPO = parsedArgs && parsedArgs.repoPath ? parsedArgs.repoPath : '/home/wayne/git/ghost'
const REAL_DB = '/home/wayne/.local/share/ghost/ghost.db'

phase('Setup')
const runId = await agent(
  'Run exactly this command and return ONLY its stdout, nothing else: date +%Y%m%d-%H%M%S-eval',
  { label: 'run-id' }
)
const trimmedRunId = runId.trim()
// The finally block's cleanup glob is /tmp/ghost-eval/${trimmedRunId}* — an
// empty or malformed value here would widen that glob to /tmp/ghost-eval/*
// and wipe every run's scratch dir, not just this one.
if (!/^\d{8}-\d{6}-eval$/.test(trimmedRunId)) {
  throw new Error(`Setup: run-id agent returned unexpected output, refusing to proceed (cleanup glob would be unsafe): ${JSON.stringify(runId)}`)
}
const scratchRoot = `/tmp/ghost-eval/${trimmedRunId}`
log(`Eval run ${trimmedRunId} — scratch root ${scratchRoot} — repo ${REPO}`)

await agent(
  `Run: mkdir -p ${scratchRoot}/data ${scratchRoot}/config && echo ready`,
  { label: 'scratch-mkdir' }
)

let report
try {
  // Later phases each mint their own unit run-id as `${runId.trim()}-<unit-key>`
  // (unit-key = replay project id, storyline key, or stress scenario key) and,
  // inside the agent prompt that does the actual live-agent work, shell out to:
  //   bash ${REPO}/docs/superpowers/eval/lib/make-unit-config.sh <unit-run-id> ${REPO}/docs/superpowers/eval/lib/ghost-wrapped ${REPO}/ghost
  //   bash ${REPO}/docs/superpowers/eval/lib/claude-eval-session.sh <unit-run-id> "<prompt>"
  // A storyline's sessions all reuse the SAME unit-run-id (config built once,
  // before session 1) so session N+1's scratch DB actually contains what
  // session N saved. Every other unit type gets its own unit-run-id.

  phase('Replay')
  const REPLAY_PROJECTS = (parsedArgs && parsedArgs.replayProjects) || ['ghost', 'roller', 'infra']
  log(`Replay projects: ${REPLAY_PROJECTS.join(', ')}`)

  const REPLAY_SCHEMA = {
    type: 'object',
    required: ['projectId', 'savedMemories', 'recall', 'precision', 'mismatches', 'frustrations', 'trustRating'],
    properties: {
      projectId: { type: 'string' },
      savedMemories: { type: 'array', items: { type: 'string' } },
      recall: { type: 'number' },
      precision: { type: 'number' },
      mismatches: { type: 'array', items: { type: 'string' } },
      frustrations: { type: 'array', items: { type: 'string' } },
      trustRating: { type: 'string' },
    },
  }

  const replayResults = await parallel(REPLAY_PROJECTS.map(projectId => async () => {
    const transcriptGlob = `/home/wayne/.claude/projects/-home-wayne-git-${projectId}*/*.jsonl`
    const exportPath = `${scratchRoot}/${projectId}-real-memories.jsonl`
    const unitRunId = `${trimmedRunId}-replay-${projectId}`

    await agent(
      `Run exactly: bash ${REPO}/docs/superpowers/eval/lib/export-memories.sh ${REAL_DB} ${projectId} ${exportPath}\n` +
      `Then run: cat ${exportPath}\n` +
      `Return the full file contents verbatim.`,
      { label: `export:${projectId}`, phase: 'Replay' }
    )

    // The actual replay work happens inside an isolated `claude -p` subprocess,
    // not via this agent's own (real, user-scoped) MCP connection. This agent's
    // job is to set up that subprocess's scratch config, launch it with the
    // replay task as its prompt, and relay back the raw stdout for grading.
    const replayPrompt =
      `You are replaying a real historical coding session for the "${projectId}" project to test whether you would ` +
      `save the same memories a real agent+human pair actually judged worth keeping. This is a live-agent evaluation, ` +
      `not a summarization task — behave exactly as you would during a real session.\n\n` +
      `1. Find the most recent transcript file matching ${transcriptGlob}. Read it.\n` +
      `2. Work through the transcript's turns as if you were living through that session right now. Ghost's MCP tools ` +
      `are wired to an isolated scratch database (never the real one) — use project_id "${projectId}-replay" for every ` +
      `ghost_* call.\n` +
      `3. Whenever something in the transcript would, in your judgment, be worth saving to memory (an architecture fact, ` +
      `a decision, a gotcha, a convention, a preference), call ghost_memory_save or ghost_decision_record exactly as you ` +
      `would live. Do not save everything indiscriminately — save what you'd actually save.\n` +
      `4. The file ${exportPath} contains the REAL memories a human+agent pair actually kept for this project (ground truth). ` +
      `Read it (via a plain shell command, e.g. cat) AFTER you finish your own save decisions, not before — don't let it ` +
      `bias your saves.\n` +
      `5. Compare your saved memories against the ground truth file. Compute recall (fraction of ground-truth memories ` +
      `you also saved, by meaning not exact text) and precision (fraction of your saves that match something in ground ` +
      `truth). List concrete mismatches (missed real memories, and noise you saved that isn't in ground truth).\n` +
      `6. Report frustration points you hit using ghost's tools during this replay, and an honest trust rating: would ` +
      `you have relied on what ghost surfaced, unverified?\n\n` +
      `End your final message with a fenced JSON block matching this shape: {"projectId": string, "savedMemories": ` +
      `string[], "recall": number, "precision": number, "mismatches": string[], "frustrations": string[], "trustRating": string}.`

    const rawOutput = await agent(
      `Run these commands in order from ${REPO}, quoting the prompt exactly as given (it contains newlines):\n` +
      `1. bash docs/superpowers/eval/lib/make-unit-config.sh ${unitRunId} $(pwd)/docs/superpowers/eval/lib/ghost-wrapped $(pwd)/ghost\n` +
      `2. bash docs/superpowers/eval/lib/claude-eval-session.sh ${unitRunId} '${replayPrompt.replace(/'/g, "'\\''")}'\n` +
      `Return the full stdout of step 2 verbatim, nothing else.`,
      { label: `replay-session:${projectId}`, phase: 'Replay' }
    )

    return agent(
      `The text below is the full transcript of an isolated \`claude -p\` replay session for project "${projectId}". ` +
      `Extract the trailing fenced JSON block it was asked to produce and re-emit it via the required schema (fill in ` +
      `"${projectId}" for projectId if the block omitted or mismatched it). If no valid JSON block is present, read the ` +
      `surrounding prose and infer the fields as best you can, and note this failure inside "frustrations".\n\n${rawOutput}`,
      { label: `replay:${projectId}`, phase: 'Replay', schema: REPLAY_SCHEMA }
    )
  }))

  phase('Consolidation')
  const CONSOLIDATION_SCHEMA = {
    type: 'object',
    required: ['projectId', 'droppedImportant', 'badMerges', 'scopeErrors', 'notes'],
    properties: {
      projectId: { type: 'string' },
      droppedImportant: { type: 'array', items: { type: 'string' } },
      badMerges: { type: 'array', items: { type: 'string' } },
      scopeErrors: { type: 'array', items: { type: 'string' } },
      notes: { type: 'string' },
    },
  }

  const consolidationResults = await pipeline(
    REPLAY_PROJECTS,
    async (projectId) => {
      const scratchDb = `${scratchRoot}/consolidation-${projectId}/data/ghost/ghost.db`
      const scratchDataHome = `${scratchRoot}/consolidation-${projectId}/data`
      const scratchConfigHome = `${scratchRoot}/consolidation-${projectId}/config`

      await agent(
        `Run these commands in order and report only the final line of output:\n` +
        `mkdir -p ${scratchDataHome} ${scratchConfigHome}\n` +
        `XDG_DATA_HOME=${scratchDataHome} XDG_CONFIG_HOME=${scratchConfigHome} ${REPO}/ghost mcp status || true\n` +
        `bash ${REPO}/docs/superpowers/eval/lib/seed-project.sh ${REAL_DB} ${projectId} ${scratchDb}\n` +
        `echo seeded`,
        { label: `seed:${projectId}`, phase: 'Consolidation' }
      )

      const reflectOutput = await agent(
        `Run exactly: XDG_DATA_HOME=${scratchDataHome} XDG_CONFIG_HOME=${scratchConfigHome} ${REPO}/ghost reflect ${projectId} --tier haiku\n` +
        `Return the full stdout verbatim (this is a dry run — no --apply — nothing is written).`,
        { label: `reflect:${projectId}`, phase: 'Consolidation' }
      )

      const realMemories = await agent(
        `Run exactly: cat ${scratchRoot}/${projectId}-real-memories.jsonl\n` +
        `Return the full file contents verbatim.`,
        { label: `real-memories:${projectId}`, phase: 'Consolidation' }
      )

      return agent(
        `You are grading a memory-consolidation run for the "${projectId}" project.\n\n` +
        `The REAL current memory set (ground truth, before consolidation) is:\n${realMemories}\n\n` +
        `The output of "ghost reflect ${projectId} --tier haiku" (a dry-run consolidation proposal) is:\n${reflectOutput}\n\n` +
        `Grade the proposal: did it drop anything from the real set that looks important (droppedImportant)? Did it ` +
        `merge things that shouldn't have been merged, losing distinct information (badMerges)? Did it mis-scope ` +
        `anything project-specific to global, or vice versa (scopeErrors)? Give a short overall note.\n\n` +
        `Return your findings via the required schema.`,
        { label: `grade-reflect:${projectId}`, phase: 'Consolidation', schema: CONSOLIDATION_SCHEMA }
      )
    }
  )

  phase('Synthesize')
  // placeholder until Task 9 fills this in
  report = { note: 'synthesis not yet implemented — see plan Task 9' }
} finally {
  // Per-unit dirs (make-unit-config.sh's UNIT_ROOT) are siblings of scratchRoot,
  // not children — e.g. /tmp/ghost-eval/<run-id>-replay-ghost — so the cleanup
  // glob must cover /tmp/ghost-eval/<run-id>* to remove them too.
  await agent(
    `Run: rm -rf /tmp/ghost-eval/${trimmedRunId}* && echo cleaned`,
    { label: 'cleanup' }
  )
}

return report
