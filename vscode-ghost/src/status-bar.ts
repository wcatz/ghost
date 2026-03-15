import * as vscode from "vscode";
import { GhostClient } from "./ghost-client";

export class GhostStatusBar {
  private item: vscode.StatusBarItem;
  private client: GhostClient;
  private connected = false;
  private mode = "";
  private tokenInfo = "";

  constructor(client: GhostClient) {
    this.client = client;
    this.item = vscode.window.createStatusBarItem(
      vscode.StatusBarAlignment.Right,
      100
    );
    this.item.command = "ghost.setMode";
    this.update();
    this.item.show();
  }

  public setClient(client: GhostClient): void {
    this.client = client;
  }

  public setConnected(connected: boolean): void {
    this.connected = connected;
    this.update();
  }

  public setMode(mode: string): void {
    this.mode = mode;
    this.update();
  }

  public setTokenInfo(input: number, output: number, cached?: number): void {
    const parts = [`in:${input}`, `out:${output}`];
    if (cached) {
      parts.push(`cache:${cached}`);
    }
    this.tokenInfo = parts.join(" ");
    this.update();
  }

  public dispose(): void {
    this.item.dispose();
  }

  private update(): void {
    if (!this.connected) {
      this.item.text = "$(debug-disconnect) Ghost: offline";
      this.item.tooltip = "Ghost daemon not connected. Click to set mode.";
      this.item.backgroundColor = undefined;
      return;
    }

    const parts = ["$(hubot) Ghost"];
    if (this.mode) {
      parts.push(this.mode);
    }
    if (this.tokenInfo) {
      parts.push(this.tokenInfo);
    }

    this.item.text = parts.join(" | ");
    this.item.tooltip = "Click to change Ghost mode";
    this.item.backgroundColor = undefined;
  }
}
