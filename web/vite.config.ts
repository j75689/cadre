import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import react from "@vitejs/plugin-react";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";

const outDir = resolve(import.meta.dirname, "../internal/web/dist");

// The build empties dist; .gitkeep goes back so a checkout keeps the
// directory go:embed needs.
const keepDist: Plugin = {
  name: "keep-dist",
  closeBundle() {
    writeFileSync(resolve(outDir, ".gitkeep"), "");
  },
};

export default defineConfig({
  plugins: [react(), keepDist],
  build: { outDir, emptyOutDir: true },
  server: {
    // npm run dev against a cadre-server on its default port. Requests
    // pass as the server's own origin, which its check on cookie-made
    // changes wants: development only.
    proxy: {
      "/api": { target: "http://localhost:8080", headers: { origin: "http://localhost:8080" } },
      "/auth": { target: "http://localhost:8080", headers: { origin: "http://localhost:8080" } },
    },
  },
  test: {
    environment: "jsdom",
  },
});
