// ghost-opencode — opencode lifecycle adapter for Ghost
// (https://github.com/wcatz/ghost). Installed and updated by
// `ghost mcp init --client opencode`; local edits are overwritten by the next
// init run.
//
// One file serves both opencode generations. V1 loads the GhostPlugin
// function (as a named export on older releases, or as the default export's
// server() on 1.18.29+ — the same reference, so it runs once); V2 calls the
// default export's setup(). The two implementations share helpers but not
// hooks — the V2 plugin API is a different surface (see setupV2 below).
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
import type { Plugin as PluginV2 } from "@opencode/plugin"
import { spawn } from "node:child_process"
import { appendFile, mkdtemp, mkdir, rm, writeFile } from "node:fs/promises"
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

// Races a promise against a deadline, resolving to `fallback` if the deadline
// wins. The loser is left to settle on its own — never awaited, never thrown
// away forcibly, just ignored. Needed because `config()` runs during
// opencode's own startup: an RPC back into the SDK client (e.g.
// client.session.list()) can depend on server state that isn't up yet, and
// without a bound it stalls config() — and therefore all of opencode's
// startup, since nothing past config() runs until it resolves — forever.
const withTimeout = <T>(promise: Promise<T>, ms: number, fallback: T): Promise<T> =>
	new Promise((resolve) => {
		const timer = setTimeout(() => resolve(fallback), ms)
		promise.then((v) => {
			clearTimeout(timer)
			resolve(v)
		}, () => {
			clearTimeout(timer)
			resolve(fallback)
		})
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

		// The plugin-level `directory` is opencode's startup cwd — often
		// $HOME for desktop launches. The session's own directory is the
		// project actually being worked in, so lifecycle spawns
		// (resolve/supersede/reflect) must resolve against it. Fail-open:
		// any lookup failure keeps the startup cwd.
		let cwd = directory ?? process.cwd()
		try {
			const info = await client.session.get({ path: { id: sessionID } })
			if (info.data?.directory) cwd = info.data.directory
		} catch {
			// keep startup cwd
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
			cwd,
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
				// Prefer the most recently used session's directory over the
				// startup cwd: desktop launches start opencode in $HOME, but
				// the injected context should describe the project the user
				// will actually resume. Empty list or lookup failure falls
				// back to the startup cwd.
				let ctxCwd = directory ?? process.cwd()
				try {
					// This RPC round-trips through opencode's own server, which
					// is still coming up during config() — bound it so a slow or
					// not-yet-ready server degrades to the startup cwd instead of
					// stalling config() (and all of opencode's startup) forever.
					const sessions = await withTimeout(client.session.list(), 1500, { data: [] as unknown[] })
					if (Array.isArray(sessions.data) && sessions.data.length > 0) {
						const recent = sessions.data.reduce((a, b) =>
							(a.time?.updated ?? 0) >= (b.time?.updated ?? 0) ? a : b)
						if (recent?.directory) ctxCwd = recent.directory
					}
				} catch {
					// keep startup cwd
				}
				const ctx = await renderStartContext(ctxCwd)
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

// ---------------------------------------------------------------------------
// opencode V2
//
// V2 differences that shape this implementation (opencode 2.0.x plugin API):
//   - no config hook: the MCP server is registered through ctx.mcp.transform,
//     and start context is appended to the system prompt by the session
//     "context" hook instead of cfg.instructions;
//   - no event hook: ctx.event.subscribe() is an async iterable, drained in
//     the background and aborted by the cleanup setup() returns;
//   - a turn ends with session.execution.{succeeded,failed,interrupted}
//     (plus session.idle / session.status idle on long-lived servers);
//   - the transcript comes from ctx.session.context and is materialized in
//     the opencode-v2-messages format;
//   - MCP tools default to Code Mode, so the save tool is reached as
//     tools.ghost.ghost_memory_save inside `execute`, not as a native tool;
//   - no app.log: diagnostics go to a best-effort file under ~/.cache/ghost.

type ContextV2 = PluginV2.Context

const V2_LOG_FILE = join(homedir(), ".cache", "ghost", "opencode-plugin.log")

const nudgePromptV2 = (reason: string): string =>
	`[Ghost] ${reason} As the assistant, if there are discoveries worth keeping, save them now with the ghost MCP server's ghost_memory_save tool (tools.ghost.ghost_memory_save in Code Mode). This is an automated reminder — act on it rather than asking the user.`

const V2_STOP_EVENTS = new Set([
	"session.execution.succeeded",
	"session.execution.failed",
	"session.execution.interrupted",
	"session.idle",
])

// The start-context block is a per-session snapshot rendered once for the
// session's own directory; the promise is cached so concurrent model requests
// share one `ghost context` spawn. FIFO-bounded like lastFire.
const startContextV2 = new Map<string, Promise<string>>()

const setupV2 = async (ctx: ContextV2) => {
	// opencode V1 (seen on 1.18.32 `run`) also calls setup() — alongside
	// server() — with a partial context that has no mcp/session/event/location
	// domains. server() already covers V1 there, so bail out rather than
	// half-run (and let the event loop spin on a missing domain).
	const partial = ctx as Partial<ContextV2> | undefined
	if (!partial?.mcp || !partial.session || !partial.event || !partial.location) return
	const ghostBin = process.env.GHOST_BIN ?? GHOST_BIN_DEFAULT
	const log = async (message: string) => {
		try {
			await mkdir(join(homedir(), ".cache", "ghost"), { recursive: true })
			await appendFile(V2_LOG_FILE, `${new Date().toISOString()} ${message}\n`)
		} catch {
			// Logging is best-effort; never let it mask the real outcome.
		}
	}

	const sessionDirectory = async (sessionID: string): Promise<string> => {
		const fallback = ctx.location.directory ?? process.cwd()
		try {
			const info = await withTimeout(ctx.session.get({ sessionID }), 1500, undefined)
			return info?.location?.directory || fallback
		} catch {
			return fallback
		}
	}

	// MCP self-registration: the plugin alone brings ghost's tools online,
	// no opencode config edit needed.
	try {
		await ctx.mcp.transform((mcp) => {
			mcp.set("ghost", { type: "local", command: [ghostBin, "mcp"], disabled: false })
		})
	} catch (e) {
		await log(`ghost: fail-open (mcp registration: ${e})`)
	}

	// Passive start context, appended to every primary request's system
	// prompt. Fail-open: an empty render adds nothing.
	try {
		await ctx.session.hook("context", async (input) => {
			let pending = startContextV2.get(input.sessionID)
			if (!pending) {
				pending = sessionDirectory(input.sessionID).then(renderStartContext).catch(() => "")
				startContextV2.set(input.sessionID, pending)
				if (startContextV2.size > MAX_TRACKED_SESSIONS) {
					const oldest = startContextV2.keys().next().value
					if (oldest !== undefined) startContextV2.delete(oldest)
				}
			}
			const text = await pending
			if (!text.trim()) return
			const hint = "\n\n---\n\n*Snapshot captured at this session's start. Memory saved after startup won't appear here — call `ghost_project_context` (or any ghost MCP tool) for the live view.*\n"
			input.system.push({ type: "text", text: text + hint })
		})
	} catch (e) {
		await log(`ghost: fail-open (context hook: ${e})`)
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
			const messages = await ctx.session.context({ sessionID })
			if (Array.isArray(messages) && messages.length > 0) {
				const dir = await mkdtemp(join(tmpdir(), "ghost-"))
				transcriptPath = join(dir, "messages.jsonl")
				await writeFile(transcriptPath, messages.map((m) => JSON.stringify(m)).join("\n") + "\n")
			}
		} catch (e) {
			await log(`ghost: fail-open (transcript materialization: ${e})`)
			transcriptPath = ""
		}

		const payload = {
			contract: {
				version: CONTRACT_VERSION,
				source: "opencode",
				transcript_format: transcriptPath ? "opencode-v2-messages" : "none",
			},
			hook_event_name: "stop",
			session_id: sessionID,
			transcript_path: transcriptPath,
			cwd: await sessionDirectory(sessionID),
			stop_hook_active: false,
		}

		try {
			const child = spawn(ghostBin, ["hook", "stop", "--source", "opencode"], {
				stdio: ["pipe", "pipe", "pipe"],
				detached: true,
				env: process.env,
			})
			child.on("error", (e) => {
				log(`ghost: fail-open (spawn: ${e})`)
			})
			// Same stdout/stderr handling as V1: the nudge ghost prints is
			// injected into the live session, and stderr is drained (never
			// inherited — issue #363) and re-routed to the log.
			let nudge = ""
			child.stdout?.on("data", (d) => { nudge += d.toString() })
			let errs = ""
			child.stderr?.on("data", (d) => { errs += d.toString() })
			child.on("close", () => {
				if (errs.trim()) log(`ghost hook stderr: ${errs.trim().slice(0, 500)}`)
				const trimmed = nudge.trim()
				if (trimmed && !nudgedSessions.has(sessionID)) {
					let reason = trimmed
					try {
						const parsed = JSON.parse(trimmed)
						if (typeof parsed?.reason === "string") reason = parsed.reason
					} catch { /* keep raw payload */ }
					nudgedSessions.set(sessionID, true)
					if (nudgedSessions.size > MAX_TRACKED_SESSIONS) {
						const oldest = nudgedSessions.keys().next().value
						if (oldest !== undefined) nudgedSessions.delete(oldest)
					}
					ctx.session.synthetic({
						sessionID,
						text: nudgePromptV2(reason),
						description: "Ghost save reminder",
						delivery: "queue",
						resume: true,
					}).catch(() => log(`ghost: ${reason}`))
				}
				if (transcriptPath) rm(join(transcriptPath, ".."), { recursive: true, force: true }).catch(() => {})
			})
			child.stdin.on("error", () => {})
			child.stdin.end(JSON.stringify(payload))
			child.unref()
		} catch (e) {
			await log(`ghost: fail-open (spawn: ${e})`)
		}
	}

	// The event stream is resubscribed whenever it ends or fails without an
	// abort (e.g. a server-side reconnect), so the stop hook never silently
	// goes dark for the rest of a long-lived server's life.
	const abort = new AbortController()
	const consume = async () => {
		for await (const event of ctx.event.subscribe({ signal: abort.signal })) {
			try {
				const isIdleStatus = event.type === "session.status" && event.data.status.type === "idle"
				if (!isIdleStatus && !V2_STOP_EVENTS.has(event.type)) continue
				const sessionID = (event.data as { sessionID?: string }).sessionID ?? ""
				if (!sessionID) continue
				// A long-lived V2 server runs one plugin instance per
				// location, and every instance receives every session's
				// events; module state is not shared between them. Only the
				// instance owning the session's location may fire, or each
				// open project would spawn its own stop hook and nudge.
				const where = event.location?.directory ?? await sessionDirectory(sessionID)
				if (where !== ctx.location.directory) continue
				await fireStopHook(sessionID)
			} catch (e) {
				await log(`ghost: fail-open (${e})`)
			}
		}
	}
	void (async () => {
		while (!abort.signal.aborted) {
			try {
				await consume()
			} catch (e) {
				if (!abort.signal.aborted) await log(`ghost: fail-open (event stream: ${e})`)
			}
			if (!abort.signal.aborted) await new Promise((r) => setTimeout(r, 1000))
		}
	})()
	return () => abort.abort()
}

export default {
	id: "ghost",
	setup: setupV2,
	server: GhostPlugin,
}
