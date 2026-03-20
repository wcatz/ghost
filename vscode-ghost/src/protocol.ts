// Shared message protocol between extension host and webview.
// Single source of truth -- imported by both sides.

// --- Extension -> Webview ---

export type ExtToWebviewMessage =
  | { type: "status"; connected: boolean }
  | { type: "session"; session: SessionInfo }
  | { type: "user_message"; text: string }
  | { type: "text_delta"; text: string }
  | { type: "thinking_delta"; text: string }
  | { type: "tool_start"; id: string; name: string }
  | { type: "tool_delta"; id: string; delta: string }
  | { type: "tool_end"; id: string; name: string }
  | { type: "tool_result"; id: string; name: string; output: string; is_error: boolean }
  | { type: "approval_required"; stream_id: string; tool_name: string; input: unknown }
  | { type: "approval_resolved" }
  | { type: "aborted"; reason: string }
  | {
      type: "done";
      session_cost: string | null;
      usage: TokenUsage | null;
      stop_reason: string;
    }
  | { type: "streaming"; active: boolean }
  | { type: "image_data"; image: ImageAttachment }
  | { type: "error"; text: string }
  | { type: "history"; messages: HistoryMessage[] }
  | { type: "send_from_command"; text: string }
  | { type: "system_message"; text: string }
  | { type: "mode_changed"; mode: string }
  | { type: "voice_error"; text: string }
  | { type: "voice_started" }
  | { type: "voice_stopped" }
  | { type: "voice_triggered" }
  | { type: "voice_partial"; text: string }
  | { type: "voice_final"; text: string }
  | { type: "monthly_cost"; text: string };

// --- Webview -> Extension ---

export type WebviewToExtMessage =
  | { type: "ready" }
  | { type: "send"; text: string; image?: ImageAttachment }
  | { type: "approve"; approved: boolean; instructions?: string }
  | { type: "abort" }
  | { type: "setMode"; mode: string }
  | { type: "set_auto_approve"; enabled: boolean }
  | { type: "attach_image" }
  | { type: "slash_command"; command: string; args?: string }
  | { type: "voice_start" }
  | { type: "voice_stop" }
  | { type: "voice_transcript"; text: string };

// --- Shared types ---

export interface SessionInfo {
  id: string;
  project_path: string;
  project_name: string;
  git_branch?: string;
  mode: string;
  active: boolean;
  messages: number;
  created_at: string;
  last_active_at: string;
}

export interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
}

export interface ImageAttachment {
  media_type: string;
  data: string; // base64
}

export interface HistoryMessage {
  role: string;
  content: string;
}
