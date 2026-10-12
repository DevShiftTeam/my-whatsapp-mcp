import { test, expect } from '@playwright/test';
import { readFile } from 'node:fs/promises';
import { pages } from '../public-pages.mjs';

const base = '/my-whatsapp-mcp/';

for (const width of [1440, 390]) {
  test(`all public guides, search, navigation and keyboard copy at ${width}px`, async ({ page, context }, testInfo) => {
    test.setTimeout(90_000);
    await page.setViewportSize({ width, height: 1000 });
    await page.emulateMedia({ reducedMotion: 'reduce', colorScheme: 'light' });
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);
    const external: string[] = [];
    const failed: string[] = [];
    page.on('pageerror', error => { failed.push(error.message); });
    page.on('request', request => { if (new URL(request.url()).origin !== 'http://127.0.0.1:4173') external.push(request.url()); });
    page.on('response', response => { if (response.status() >= 400) failed.push(`${response.status()} ${response.url()}`); });
    for (const guide of pages) {
      await page.goto(`${base}${guide.route}.html`);
      await expect(page.locator('.vp-doc h1')).toBeVisible();
      await expect(page.getByText('Qualification record', { exact: true })).toHaveCount(0);
      await expect(page.locator('.vp-doc')).not.toContainText('documentation_reviewed');
      if (guide.section === 'Installation') {
        await expect(page.getByRole('region', { name: 'Setup status' })).toContainText('Preview setup');
      }
      if (guide.route === 'index' || guide.route === 'everyday/search') {
        await expect(page.getByRole('region', { name: 'Setup status' })).toHaveCount(0);
      }
      for (const [rel, file, type] of [
        ['icon', 'favicon.ico', 'image/x-icon'],
        ['icon', 'favicon.png', 'image/png'],
        ['apple-touch-icon', 'apple-touch-icon.png', 'image/png']
      ]) {
        const href = `${base}assets/${file}`;
        await expect(page.locator(`head link[rel="${rel}"][href="${href}"]`)).toHaveCount(1);
        if (guide.route === 'index') {
          const icon = await page.request.get(href);
          expect(icon.ok()).toBeTruthy();
          expect(icon.headers()['content-type']).toBe(type);
          expect((await icon.body()).length).toBeGreaterThan(100);
        }
      }
      const download = page.getByRole('link', { name: 'Download Markdown', exact: true });
      await expect(download).toHaveAttribute('href', `${base}markdown/${guide.route}.md`);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy();
    }
    await page.goto(`${base}install/docker-compose.html`);
    await page.reload();
    await expect(page.locator('.vp-doc h1')).toHaveText('Docker Compose');

    // The index route is the regression case: both navigation and search must
    // preserve /remote/index.html, without directory fallback in the server.
    if (width === 390) await page.getByRole('button', { name: 'Menu', exact: true }).click();
    await page.locator('#VPSidebarNav').getByRole('link', { name: 'Remote access', exact: true }).click();
    await expect(page).toHaveURL(`${base}remote/index.html`);
    await page.reload();
    await expect(page).toHaveURL(`${base}remote/index.html`);
    await expect(page.locator('.vp-doc h1')).toContainText('Remote');
    await page.getByRole('button', { name: 'Search', exact: true }).click();
    await page.getByRole('searchbox').fill('Remote access');
    const result = page.locator('.VPLocalSearchBox a[href*="/remote/index.html"]').first();
    await expect(result).toBeVisible();
    await result.click();
    await expect(page).toHaveURL(/\/remote\/index\.html(?:#.*)?$/);
    const selectedURL = page.url();
    await page.locator('.vp-doc').getByRole('link', { name: 'Tailscale Serve', exact: true }).click();
    await expect(page).toHaveURL(/\/remote\/tailscale-serve\.html$/);
    await page.goBack();
    await expect(page).toHaveURL(selectedURL);
    await page.goForward();
    await expect(page).toHaveURL(/\/remote\/tailscale-serve\.html$/);
    await page.goBack();
    await expect(page).toHaveURL(selectedURL);
    await page.reload();
    await expect(page).toHaveURL(selectedURL);

    await page.goto(`${base}clients/codex.html`);
    const heading = page.locator('.vp-doc h2 .header-anchor').first();
    await heading.focus();
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/#local-stdio$/);
    await page.keyboard.press('Tab');
    await expect(page.locator('.vp-doc button.copy').first()).toBeFocused();
    await page.keyboard.press('Enter');
    const code = await page.locator('.vp-doc div[class*="language-"] pre code').first().innerText();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(code.trimEnd());
    const copy = page.getByRole('button', { name: 'Copy page as Markdown' });
    await copy.focus();
    await page.keyboard.press('Enter');
    const exported = await readFile(new URL('../dist/markdown/clients/codex.md', import.meta.url), 'utf8');
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(exported);

    if (width === 390) await page.getByRole('button', { name: 'mobile navigation', exact: true }).click();
    const appearance = page.getByRole('switch').filter({ visible: true }).first();
    await appearance.click();
    await expect(page.locator('html')).toHaveClass(/dark/);
    if (width === 390) await page.getByRole('button', { name: 'mobile navigation', exact: true }).click();
    if (width === 390) await expect(page.locator('.VPNavScreen')).toBeHidden();
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: testInfo.outputPath(`docs-${width}-dark.png`), fullPage: true, animations: 'disabled' });
    if (width === 390) await page.getByRole('button', { name: 'mobile navigation', exact: true }).click();
    await appearance.click();
    await expect(page.locator('html')).not.toHaveClass(/dark/);
    if (width === 390) await page.getByRole('button', { name: 'mobile navigation', exact: true }).click();
    if (width === 390) await expect(page.locator('.VPNavScreen')).toBeHidden();
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: testInfo.outputPath(`docs-${width}-light.png`), fullPage: true, animations: 'disabled' });
    expect(external, 'No external fonts, analytics, or other automatic requests').toEqual([]);
    expect(failed, 'All actual browser resource requests resolved').toEqual([]);
  });
}
