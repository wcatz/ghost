// ghost-opencode v1 — opencode lifecycle adapter for Ghost
// (https://github.com/wcatz/ghost). Installed and updated by
// `ghost mcp init --client opencode`; local edits are overwritten by the next
// init run.
//
// Bridges opencode's idle transition to the ghost host-event contract:
//
//	ghost hook stop --source opencode
//
// with the transcript materialized as temp JSONL in the `opencode-messages`
// format ({info, parts} per line, verbatim client.session.messages
// serialization). Ghost scans it for save-tool usage. opencode cannot block
// a stop, so the save-nudge is injected into the live session via
// client.session.promptAsync (the faithful analog of the claude/codex blocking
// nudge) so the agent itself acts on it; failures fall back to a log line.
// fail-open is absolute: every error logs one line and never disturbs the
// session.
import type { Plugin } from "@opencode-ai/plugin"
import { spawn } from "node:child_process"
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises"
import { homedir, tmpdir } from "node:os"
import { join } from "node:path"

const CONTRACT_VERSION = 1

// Replaced by the installer with the absolute ghost binary path it resolved
// at install time. Desktop launchers often run opencode with a narrower PATH
// than the shell that ran `ghost mcp init`, so the default must not rely on
// lookup; GHOST_BIN remains the higher-priority override for hermetic setups.
const GHOST_BIN_DEFAULT = "__GHOST_BIN__"

// Once a modern session.status event has been observed, legacy session.idle
// events are ignored — versions emitting both would otherwise double-fire.
let sawStatusEvent = false

// Idle transitions can repeat in quick succession (status + legacy idle, or
// rapid turns); one stop hook per session per window is enough. The map is
// FIFO-bounded: long-lived hosts (desktop apps) would otherwise grow one
// entry per session forever. JS Maps iterate in insertion order, so the
// oldest entry is evicted.
const lastFire = new Map<string, number>()
const DEBOUNCE_MS = 2000
const MAX_TRACKED_SESSIONS = 256

// Once the save reminder has been injected into a session, don't re-inject it
// on later idle transitions: the nudge condition stays true until something is
// actually saved, and re-prompting every idle would be noisy. Bounded like
// lastFire so long-lived hosts don't grow it without limit.
const nudgedSessions = new Map<string, true>()

// Builds the agent-facing instruction injected into the live session when the
// save nudge fires: a clear directive to review the session and persist any
// discoveries via ghost_memory_save.
const nudgePrompt = (reason: string): string =>
	`[Ghost] ${reason} As the assistant, if there are discoveries worth keeping, save them now via ghost_ghost_memory_save. This is an automated reminder — act on it rather than asking the user.`

// Materializes ghost's session-start context block for a directory and returns
// it, so opencode can inject it passively via instructions (opencode has no
// stdout-injection surface of its own). Read-only and fail-open: any spawn
// error or missing context yields "" and the caller skips injection. stderr is
// ignored — this helper runs outside the plugin closure (no app.log access),
// and ghost context diagnostics must never reach the terminal (issue #363).
const renderStartContext = (cwd: string): Promise<string> =>
	new Promise((resolve) => {
		const child = spawn(process.env.GHOST_BIN ?? GHOST_BIN_DEFAULT, ["context", "--cwd", cwd], {
			stdio: ["ignore", "pipe", "ignore"],
		})
		let out = ""
		child.stdout?.on("data", (d) => {
			out += d.toString()
		})
		child.on("error", () => resolve(""))
		child.on("close", () => resolve(out))
	})

export const GhostPlugin: Plugin = async ({ client, directory }) => {
	const log = async (level: "info" | "warn" | "error", message: string) => {
		try {
			await client.app.log({ body: { service: "ghost-opencode", level, message } })
		} catch {
			// Logging is best-effort; never let it mask the real outcome.
		}
	}

	const fireStopHook = async (sessionID: string) => {
		if (!sessionID) return
		const now = Date.now()
		if (now - (lastFire.get(sessionID) ?? 0) < DEBOUNCE_MS) return
		lastFire.set(sessionID, now)
		if (lastFire.size > MAX_TRACKED_SESSIONS) {
			const oldest = lastFire.keys().next().value
			if (oldest !== undefined) lastFire.delete(oldest)
		}

		let transcriptPath = ""
		try {
			const res = await client.session.messages({ path: { id: sessionID } })
			if (res.error) throw res.error
			if (Array.isArray(res.data) && res.data.length > 0) {
				const dir = await mkdtemp(join(tmpdir(), "ghost-"))
				transcriptPath = join(dir, "messages.jsonl")
				const body = res.data.map((m: unknown) => JSON.stringify(m)).join("\n") + "\n"
				await writeFile(transcriptPath, body)
			}
		} catch (e) {
			await log("warn", `ghost: fail-open (transcript materialization: ${e})`)
			transcriptPath = ""
		}

		const payload = {
			contract: {
				version: CONTRACT_VERSION,
				source: "opencode",
				transcript_format: transcriptPath ? "opencode-messages" : "none",
			},
			hook_event_name: "stop",
			session_id: sessionID,
			transcript_path: transcriptPath,
			cwd: directory ?? process.cwd(),
			stop_hook_active: false,
		}

		try {
			const child = spawn(process.env.GHOST_BIN ?? GHOST_BIN_DEFAULT, ["hook", "stop", "--source", "opencode"], {
				stdio: ["pipe", "pipe", "pipe"],
				detached: true,
				env: process.env,
			})
			child.on("error", async (e) => {
				await log("warn", `ghost: fail-open (spawn: ${e})`)
			})
			// opencode cannot block a stop, so the {"decision":"approve"} nudge
			// ghost emits on stdout is captured here and injected into the live
			// session (client.session.promptAsync) so the agent itself acts on
			// it — the faithful analog of the claude/codex blocking nudge. If
			// the injection fails it falls back to a log line.
			let nudge = ""
			child.stdout?.on("data", (d) => { nudge += d.toString() })
			// stderr is piped and drained rather than inherited: this child is
			// detached from opencode's process tree, so an inherited stderr
			// writes straight to the controlling terminal outside the TUI
			// redraw (issue #363). Diagnostics are re-routed to app.log on
			// close; if this process exits before the child does, the drain is
			// lost — acceptable for fail-open diagnostics.
			let errs = ""
			child.stderr?.on("data", (d) => { errs += d.toString() })
			// Best-effort local cleanup when we outlive the hook; ghost also
			// sweeps its ghost-* temp transcript dirs consumer-side, covering
			// hosts that exit before this handler runs (e.g. `opencode run`).
			child.on("close", () => {
				if (errs.trim()) {
					log("warn", `ghost hook stderr: ${errs.trim().slice(0, 500)}`)
				}
				const trimmed = nudge.trim()
				if (trimmed) {
					let reason = trimmed
					try {
						const parsed = JSON.parse(trimmed)
						if (typeof parsed?.reason === "string") reason = parsed.reason
					} catch { /* keep raw payload */ }
					// Inject the reminder into the live session so the agent
					// acts on it. Once per session; on failure, fall back to a
					// log line so the nudge is never silently lost.
					if (sessionID && !nudgedSessions.has(sessionID)) {
						nudgedSessions.set(sessionID, true)
						if (nudgedSessions.size > MAX_TRACKED_SESSIONS) {
							const oldest = nudgedSessions.keys().next().value
							if (oldest !== undefined) nudgedSessions.delete(oldest)
						}
						client.session.promptAsync({
							path: { id: sessionID },
							body: { parts: [{ type: "text", text: nudgePrompt(reason) }] },
						})
							.then((r) => { if (r.error) log("warn", `ghost: ${reason}`) })
							.catch(() => log("warn", `ghost: ${reason}`))
					}
				}
				if (transcriptPath) rm(join(transcriptPath, ".."), { recursive: true, force: true }).catch(() => {})
			})
			child.stdin.on("error", () => {})
			child.stdin.end(JSON.stringify(payload))
			child.unref()
		} catch (e) {
			await log("warn", `ghost: fail-open (spawn: ${e})`)
		}
	}

	return {
		// Self-registration: this hook receives the full SDK config, so the
		// plugin alone brings ghost's MCP tools online — no opencode.json edit
		// needed. GHOST_BIN overrides the baked-in absolute path (hermetic
		// setups); without it the installer-resolved path is used.
		config: async (cfg) => {
			cfg.mcp = cfg.mcp ?? {}
			cfg.mcp["ghost"] = {
				type: "local",
				command: [process.env.GHOST_BIN ?? GHOST_BIN_DEFAULT, "mcp"],
				enabled: true,
			}
			// Passive start context: ghost's session-start block is materialized
			// into a cache file and injected via opencode's instructions, so
			// every session opens with project memory without an agent action.
			// opencode cannot consume the hook's stdout injection, so this is
			// the supported path. Fail-open: any error leaves cfg untouched.
			try {
				const ctx = await renderStartContext(directory ?? process.cwd())
				if (ctx && ctx.trim()) {
					const dir = join(homedir(), ".cache", "ghost")
					await mkdir(dir, { recursive: true })
					const file = join(dir, "opencode-context.md")
					// The block is a startup snapshot (opencode has no resume/
					// compact re-injection like claude), so flag its staleness
					// locally — without touching formatSessionContext, which
					// claude/codex consume verbatim.
					const ctxHint = "\n\n---\n\n*Snapshot captured at this session's start. Memory saved after startup won't appear here — call `ghost_project_context` (or any `ghost_*` MCP tool) for the live view.*\n"
					await writeFile(file, ctx + ctxHint)
					cfg.instructions = cfg.instructions ?? []
					if (!cfg.instructions.includes(file)) cfg.instructions.push(file)
				}
			} catch {
				// fail-open: never block opencode startup over missing context
			}
		},
		event: async ({ event }) => {
			try {
				if (event.type === "session.status") {
					sawStatusEvent = true
					const props = event.properties as { sessionID?: string; status?: { type?: string } }
					if (props?.status?.type !== "idle") return
					await fireStopHook(props.sessionID ?? "")
					return
				}
				if (event.type === "session.idle" && !sawStatusEvent) {
					const props = event.properties as { sessionID?: string }
					await fireStopHook(props?.sessionID ?? "")
				}
			} catch (e) {
				await log("warn", `ghost: fail-open (${e})`)
			}
		},
	}
}
