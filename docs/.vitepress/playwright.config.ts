import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  testMatch: '**/*.spec.ts',
  fullyParallel: false,
  workers: 1,
  use: { baseURL: 'http://127.0.0.1:4173', browserName: 'chromium', trace: 'retain-on-failure' },
  webServer: {
    command: 'node scripts/preview.mjs --port 4173',
    url: 'http://127.0.0.1:4173/my-whatsapp-mcp/install/docker-compose.html',
    reuseExistingServer: false,
    timeout: 15_000
  }
});
