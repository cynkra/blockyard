import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  // Default is 30s per test. The hello-postgres stack has noticeably
  // more startup weight than hello-shiny did (Postgres init, board-
  // storage preflight, vault DB engine), so cold tests that do a
  // full OIDC round-trip in beforeEach can brush up against the
  // default on contended CI runners. 60s is still a tight budget;
  // tests that legitimately need more should call test.setTimeout.
  timeout: 60_000,
  reporter: process.env.CI
    ? [
        ["junit", { outputFile: "results/junit.xml" }],
        ["html", { open: "never" }],
      ]
    : [["html", { open: "on-failure" }]],
  use: {
    baseURL: "http://localhost:8080",
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    actionTimeout: 60_000,
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
