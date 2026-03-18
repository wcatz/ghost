import * as vscode from "vscode";

export class GhostStatusBar {
  private item: vscode.StatusBarItem;
  private connected = false;
  private mode = "";
  private cost = "";

  constructor() {
    this.item = vscode.window.createStatusBarItem(
      vscode.StatusBarAlignment.Right,
      100
    );
    this.item.command = "ghost.setMode";
    this.update();
    this.item.show();
  }

  public setConnected(connected: boolean): void {
    this.connected = connected;
    this.update();
  }

  public setMode(mode: string): void {
    this.mode = mode;
    this.update();
  }

  public setCost(cost: string): void {
    this.cost = cost;
    this.update();
  }

  public getCost(): string {
    return this.cost;
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
    if (this.cost) {
      parts.push(this.cost);
    }

    this.item.text = parts.join(" | ");
    this.item.tooltip = "Click to change Ghost mode";
    this.item.backgroundColor = undefined;
  }
}
