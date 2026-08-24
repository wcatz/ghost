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
// stops or inject context, so any ghost nudge degrades to a log line there —
// fail-open is absolute: every error logs one line and never disturbs the
// session.
import type { Plugin } from "@opencode-ai/plugin"
import { spawn } from "node:child_process"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
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
				stdio: ["pipe", "ignore", "inherit"],
				detached: true,
				env: process.env,
			})
			child.on("error", async (e) => {
				await log("warn", `ghost: fail-open (spawn: ${e})`)
			})
			// Best-effort local cleanup when we outlive the hook; ghost also
			// sweeps its ghost-* temp transcript dirs consumer-side, covering
			// hosts that exit before this handler runs (e.g. `opencode run`).
			child.on("close", () => {
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
