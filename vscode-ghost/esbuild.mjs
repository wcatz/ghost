import * as esbuild from "esbuild";

const isWatch = process.argv.includes("--watch");

const extensionConfig = {
  entryPoints: ["src/extension.ts"],
  bundle: true,
  outfile: "out/extension.js",
  external: ["vscode"],
  format: "cjs",
  platform: "node",
  target: "node18",
  sourcemap: true,
};

const webviewConfig = {
  entryPoints: ["src/webview/chat-main.ts"],
  bundle: true,
  outfile: "out/webview/chat.js",
  format: "iife",
  platform: "browser",
  target: "es2022",
  sourcemap: true,
};

if (isWatch) {
  const ctx1 = await esbuild.context(extensionConfig);
  const ctx2 = await esbuild.context(webviewConfig);
  await Promise.all([ctx1.watch(), ctx2.watch()]);
  console.log("watching...");
} else {
  await Promise.all([
    esbuild.build(extensionConfig),
    esbuild.build(webviewConfig),
  ]);
  console.log("build complete");
}
