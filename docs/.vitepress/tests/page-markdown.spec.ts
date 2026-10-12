import { test, expect } from '@playwright/test';
import { readFile } from 'node:fs/promises';

const route = '/my-whatsapp-mcp/install/docker-compose.html';
const markdown = '/my-whatsapp-mcp/markdown/install/docker-compose.md';

test('nested built guide exposes exact allowlisted Markdown and qualification', async ({ page, request }) => {
  await page.goto(route);
  await expect(page.getByRole('heading', { name: /Docker Compose/, level: 1 })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Guide qualification' })).toContainText('Preview');
  await expect(page.getByRole('region', { name: 'Guide qualification' })).toContainText('Documentation reviewed');
  await expect(page.getByRole('link', { name: 'View Markdown', exact: true })).toHaveAttribute('href', markdown);
  const download = page.getByRole('link', { name: 'Download Markdown', exact: true });
  await expect(download).toHaveAttribute('href', markdown);
  await expect(download).toHaveAttribute('download', 'wa-indexer-install-docker-compose.md');
  const response = await request.get(markdown);
  expect(response.ok()).toBeTruthy();
  const exported = await response.text();
  expect(exported).toContain('Verification status: `documentation_reviewed`');
  await page.getByText('Qualification record', { exact: true }).click();
  const visibleRecord = JSON.parse(await page.getByRole('region', { name: 'Guide qualification' }).locator('code').innerText());
  const exportedRecord = JSON.parse(exported.match(/```json\n([\s\S]*?)\n```/)![1]);
  expect(visibleRecord).toEqual(exportedRecord);
  expect(visibleRecord.availability).toBe('preview');
  expect(visibleRecord.tests).toEqual([]);
  const downloaded = page.waitForEvent('download');
  await download.click();
  const artifact = await downloaded;
  expect(artifact.suggestedFilename()).toBe('wa-indexer-install-docker-compose.md');
  expect(await readFile((await artifact.path())!, 'utf8')).toBe(exported);
  await page.getByRole('link', { name: 'View Markdown', exact: true }).click();
  await expect(page).toHaveURL(new RegExp(markdown.replaceAll('.', '\\.')));
  await expect(page.locator('body')).toContainText('# Docker Compose');
});

test('copy writes the exact fetched Markdown only after a click', async ({ page, request }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async (value: string) => { (window as any).__copied = value; } }, configurable: true });
  });
  await page.goto(route);
  expect(await page.evaluate(() => (window as any).__copied)).toBeUndefined();
  await page.getByRole('button', { name: 'Copy page as Markdown' }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Markdown copied.' })).toBeVisible();
  expect(await page.evaluate(() => (window as any).__copied)).toBe(await (await request.get(markdown)).text());
});

test('clipboard denial provides focused selected text and preserves it on fetch failure', async ({ page, request }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async () => { throw new Error('Denied'); } }, configurable: true });
  });
  await page.goto(route);
  await page.getByRole('button', { name: 'Copy page as Markdown' }).click();
  const fallback = page.getByRole('textbox', { name: 'Page Markdown for manual copying' });
  await expect(fallback).toBeFocused();
  const expected = await (await request.get(markdown)).text();
  await expect(fallback).toHaveValue(expected);
  expect(await fallback.evaluate((node: HTMLTextAreaElement) => node.selectionEnd - node.selectionStart)).toBe(expected.length);
  await page.keyboard.press('Tab');
  await expect(page.getByRole('button', { name: 'Select Markdown text' })).toBeFocused();
  await page.route('**/markdown/install/docker-compose.md', intercepted => intercepted.fulfill({ status: 503, body: 'Unavailable' }));
  await page.getByRole('button', { name: 'Copy page as Markdown' }).click();
  await expect(page.getByRole('status').filter({ hasText: 'Could not load Markdown.' })).toBeVisible();
  await expect(fallback).toHaveValue(expected);
});

test('copy is disabled while loading and private pages cannot get export controls', async ({ page }) => {
  await page.goto(route);
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  await page.route('**/markdown/install/docker-compose.md', async intercepted => { await gate; await intercepted.continue(); });
  const copy = page.getByRole('button', { name: 'Copy page as Markdown' });
  await copy.click();
  await expect(copy).toBeDisabled();
  release();
  await expect(copy).toBeEnabled();
  await page.goto('/my-whatsapp-mcp/private.html');
  await expect(page.getByRole('button', { name: 'Copy page as Markdown' })).toHaveCount(0);
});

for (const mode of ['light', 'dark', 'mobile'] as const) {
  test(`built nested page visual proof: ${mode}`, async ({ page }, testInfo) => {
    await page.setViewportSize(mode === 'mobile' ? { width: 390, height: 844 } : { width: 1440, height: 1100 });
    await page.emulateMedia({ colorScheme: mode === 'dark' ? 'dark' : 'light', reducedMotion: 'reduce' });
    await page.goto(route);
    await expect(page.getByRole('heading', { name: /Docker Compose/, level: 1 })).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy();
    await page.screenshot({ path: testInfo.outputPath(`docker-compose-${mode}.png`), fullPage: true });
  });
}


test('phone guide remains unverified, readable and exactly copyable on mobile', async ({page,request},testInfo)=>{
 await page.setViewportSize({width:390,height:844});
 await page.addInitScript(()=>Object.defineProperty(navigator,'clipboard',{value:{writeText:async(value:string)=>{(window as any).__phoneMarkdown=value;}},configurable:true}));
 await page.goto('/my-whatsapp-mcp/remote/phone-access.html');
 await expect(page.getByRole('heading',{name:'Phone access',level:1})).toBeVisible();
 await expect(page.getByRole('region',{name:'Guide qualification'})).toContainText('Documentation reviewed');
 await expect(page.locator('.vp-doc')).toContainText('not been tested');
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBeTruthy();
 const copy=page.getByRole('button',{name:'Copy page as Markdown'});await copy.focus();await page.keyboard.press('Enter');
 const response=await request.get('/my-whatsapp-mcp/markdown/remote/phone-access.md');expect(response.ok()).toBeTruthy();
 await expect.poll(()=>page.evaluate(()=>(window as any).__phoneMarkdown)).toBe(await response.text());
 await page.screenshot({path:testInfo.outputPath('phone-guide-390.png'),fullPage:true});
});
