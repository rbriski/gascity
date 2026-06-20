import { defineConfig } from "vitest/config";

// Single-bundle build: one JS, one CSS, one HTML. The Go static server
// embeds dist/ via go:embed and ships it verbatim, so predictable asset
// filenames (no hashing) keep the embedding simple.
export default defineConfig({
  build: {
    outDir: "dist",
    emptyOutDir: true,
    assetsDir: ".",
    rollupOptions: {
      output: {
        entryFileNames: "dashboard.js",
        chunkFileNames: "dashboard.js",
        assetFileNames: "[name][extname]",
      },
    },
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  test: {
    exclude: ["tests/e2e/**", "node_modules/**", "dist/**"],
  },
});
