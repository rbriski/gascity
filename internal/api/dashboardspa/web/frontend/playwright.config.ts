import { defineConfig, devices } from '@playwright/test';

const baseURL = process.env.DASHPORT_BASE_URL;
if (baseURL === undefined || baseURL === '') {
  throw new Error('DASHPORT_BASE_URL is required; run make dashboard-e2e-play');
}

const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE;

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: true,
  workers: 1,
  reporter: 'list',
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR ?? 'test-results',
  timeout: 45_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL,
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        launchOptions:
          executablePath === undefined || executablePath === '' ? {} : { executablePath },
      },
    },
  ],
});
