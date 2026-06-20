import { defineConfig } from "@playwright/test";

export default defineConfig({
  expect: { timeout: 5_000 },
  testDir: "./tests/e2e",
  timeout: 30_000,
  use: {
    browserName: "chromium",
    channel: process.env.PLAYWRIGHT_CHANNEL,
    headless: true,
    trace: "on-first-retry",
    viewport: { width: 1280, height: 900 },
  },
  workers: 1,
});
