// The capture's Playwright config: `vybava ui-loop run` points the repo's own
// Playwright at this file (-c) with UILOOP_RUN set, so a repo needs no config
// of its own for the loop.
//
// GPU path: the capture composites the way real Chrome does (SwiftShader where
// there is no GPU). The headless shell's default software compositor applies
// `backdrop-filter` with transparent edges — the unfiltered page shows through
// every dialog, sheet and menu as a sharp ghost, so glass never renders in a
// shot. Never drop these flags; capture.ts's glass probe fails loudly when
// they stop working.

import { defineConfig } from '@playwright/test';

import { loadRun, passPath } from './run';

const run = loadRun();
const executablePath = process.env['PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH'];

export default defineConfig({
  testMatch: /capture\.spec\.ts$/,
  globalSetup: './setup.ts',
  globalTeardown: './teardown.ts',
  outputDir: passPath(run, 'playwright'),
  fullyParallel: true,
  workers: run.workers,
  retries: 0,
  timeout: 180_000,
  reporter: [['list']],
  use: {
    trace: 'off',
    video: 'off',
    // The pass writes its own shots; Playwright's artefacts would only duplicate them.
    screenshot: 'off',
    launchOptions: {
      args: ['--enable-gpu', '--use-angle=swiftshader'],
      ...(executablePath ? { executablePath } : {}),
    },
  },
  projects: [{ name: 'ui-loop' }],
});
