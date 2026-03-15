import * as http from "http";
import * as https from "https";
import { URL } from "url";
import { EventEmitter } from "events";

// --- Types ---

export interface Session {
  id: string;
  project_path: string;
  project_name: string;
  mode: string;
  active: boolean;
  messages: number;
  created_at: string;
  last_active_at: string;
}

export interface Memory {
  id: string;
  project_id: string;
  category: string;
  content: string;
  source: string;
  importance: number;
  tags: string[];
  access_count: number;
  created_at: string;
  updated_at: string;
}

export interface Project {
  id: string;
  path: string;
  name: string;
}

export interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens?: number;
  cache_read_input_tokens?: number;
}

export interface StreamEvent {
  type: string;
  data: Record<string, unknown>;
}

export interface ApprovalRequest {
  tool_name: string;
  input: Record<string, unknown>;
}

// --- Client ---

export class GhostClient extends EventEmitter {
  private baseUrl: string;
  private authToken: string;

  constructor(baseUrl: string, authToken: string = "") {
    super();
    this.baseUrl = baseUrl.replace(/\/$/, "");
    this.authToken = authToken;
  }

  // --- Health ---

  async health(): Promise<{ status: string; version: string }> {
    return this.request("GET", "/api/v1/health");
  }

  async isAvailable(): Promise<boolean> {
    try {
      const resp = await this.health();
      return resp.status === "ok";
    } catch {
      return false;
    }
  }

  // --- Sessions ---

  async createSession(path: string, mode?: string, resume?: boolean): Promise<Session> {
    return this.request("POST", "/api/v1/sessions", { path, mode, resume });
  }

  async getHistory(sessionId: string): Promise<{ role: string; content: string }[]> {
    return this.request("GET", `/api/v1/sessions/${sessionId}/history`);
  }

  async listSessions(): Promise<Session[]> {
    return this.request("GET", "/api/v1/sessions");
  }

  async deleteSession(id: string): Promise<void> {
    await this.request("DELETE", `/api/v1/sessions/${encodeURIComponent(id)}`);
  }

  async setMode(sessionId: string, mode: string): Promise<{ mode: string }> {
    return this.request(
      "POST",
      `/api/v1/sessions/${encodeURIComponent(sessionId)}/mode`,
      { mode }
    );
  }

  async approve(
    sessionId: string,
    approved: boolean
  ): Promise<{ status: string }> {
    return this.request(
      "POST",
      `/api/v1/sessions/${encodeURIComponent(sessionId)}/approve`,
      { approved }
    );
  }

  async setAutoApprove(
    sessionId: string,
    enabled: boolean
  ): Promise<{ auto_approve: boolean }> {
    return this.request(
      "POST",
      `/api/v1/sessions/${encodeURIComponent(sessionId)}/auto-approve`,
      { enabled }
    );
  }

  // --- Chat (SSE streaming) ---

  sendMessage(
    sessionId: string,
    message: string,
    image?: { media_type: string; data: string }
  ): { events: EventEmitter; abort: () => void } {
    const emitter = new EventEmitter();
    const url = new URL(
      `/api/v1/sessions/${encodeURIComponent(sessionId)}/send`,
      this.baseUrl
    );

    const payload: Record<string, unknown> = { message };
    if (image) {
      payload.images = [{ type: "base64", media_type: image.media_type, data: image.data }];
    }
    const body = JSON.stringify(payload);
    const isHttps = url.protocol === "https:";
    const mod = isHttps ? https : http;

    const options: http.RequestOptions = {
      method: "POST",
      hostname: url.hostname,
      port: url.port,
      path: url.pathname,
      headers: {
        "Content-Type": "application/json",
        "Content-Length": Buffer.byteLength(body),
        Accept: "text/event-stream",
        ...this.authHeaders(),
      },
    };

    const req = mod.request(options, (res) => {
      if (res.statusCode !== 200) {
        let data = "";
        res.on("data", (chunk: Buffer) => (data += chunk.toString()));
        res.on("end", () => {
          emitter.emit("error", new Error(`HTTP ${res.statusCode}: ${data}`));
        });
        return;
      }

      let buffer = "";
      let currentEvent = "";

      res.on("data", (chunk: Buffer) => {
        buffer += chunk.toString();
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";

        for (const line of lines) {
          if (line.startsWith("event: ")) {
            currentEvent = line.slice(7).trim();
          } else if (line.startsWith("data: ")) {
            const dataStr = line.slice(6);
            try {
              const data = JSON.parse(dataStr);
              const event: StreamEvent = { type: currentEvent, data };
              emitter.emit("event", event);

              // Emit typed events for convenience.
              switch (currentEvent) {
                case "text":
                  emitter.emit("text", data.text || "");
                  break;
                case "thinking":
                  emitter.emit("thinking", data.text || "");
                  break;
                case "tool_use_start":
                  emitter.emit("tool_start", data);
                  break;
                case "tool_input_delta":
                  emitter.emit("tool_delta", data);
                  break;
                case "tool_use_end":
                  emitter.emit("tool_end", data);
                  break;
                case "approval_required":
                  emitter.emit("approval", data as ApprovalRequest);
                  break;
                case "approval_resolved":
                  emitter.emit("approval_resolved", {});
                  break;
                case "done":
                  emitter.emit("done", data);
                  break;
                case "error":
                  emitter.emit("error", new Error(data.error || "unknown"));
                  break;
              }
            } catch {
              // Skip malformed JSON lines.
            }
          }
        }
      });

      res.on("end", () => {
        emitter.emit("close");
      });

      res.on("error", (err: Error) => {
        emitter.emit("error", err);
      });
    });

    req.on("error", (err: Error) => {
      emitter.emit("error", err);
    });

    req.write(body);
    req.end();

    return {
      events: emitter,
      abort: () => req.destroy(),
    };
  }

  // --- Memories ---

  async searchMemories(
    projectId: string,
    query: string,
    limit: number = 20
  ): Promise<Memory[]> {
    return this.request("POST", "/api/v1/memories/search", {
      project_id: projectId,
      query,
      limit,
    });
  }

  async listMemories(projectId: string): Promise<Memory[]> {
    return this.request(
      "GET",
      `/api/v1/memories/${encodeURIComponent(projectId)}`
    );
  }

  async createMemory(
    projectId: string,
    category: string,
    content: string,
    importance: number = 0.5,
    tags: string[] = []
  ): Promise<{ id: string; merged: boolean }> {
    return this.request("POST", "/api/v1/memories", {
      project_id: projectId,
      category,
      content,
      source: "vscode",
      importance,
      tags,
    });
  }

  async deleteMemory(memoryId: string): Promise<void> {
    await this.request(
      "DELETE",
      `/api/v1/memories/${encodeURIComponent(memoryId)}`
    );
  }

  // --- Projects ---

  async listProjects(): Promise<Project[]> {
    return this.request("GET", "/api/v1/projects");
  }

  // --- Internal ---

  private authHeaders(): Record<string, string> {
    if (this.authToken) {
      return { Authorization: `Bearer ${this.authToken}` };
    }
    return {};
  }

  private request<T>(
    method: string,
    path: string,
    body?: unknown
  ): Promise<T> {
    return new Promise((resolve, reject) => {
      const url = new URL(path, this.baseUrl);
      const isHttps = url.protocol === "https:";
      const mod = isHttps ? https : http;

      const options: http.RequestOptions = {
        method,
        hostname: url.hostname,
        port: url.port,
        path: url.pathname + url.search,
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json",
          ...this.authHeaders(),
        },
      };

      const req = mod.request(options, (res) => {
        let data = "";
        res.on("data", (chunk: Buffer) => (data += chunk.toString()));
        res.on("end", () => {
          if (res.statusCode && res.statusCode >= 400) {
            try {
              const err = JSON.parse(data);
              reject(new Error(err.error || `HTTP ${res.statusCode}`));
            } catch {
              reject(new Error(`HTTP ${res.statusCode}: ${data}`));
            }
            return;
          }
          try {
            resolve(JSON.parse(data) as T);
          } catch {
            resolve(undefined as T);
          }
        });
      });

      req.on("error", reject);

      if (body) {
        const bodyStr = JSON.stringify(body);
        req.setHeader("Content-Length", Buffer.byteLength(bodyStr));
        req.write(bodyStr);
      }

      req.end();
    });
  }
}
