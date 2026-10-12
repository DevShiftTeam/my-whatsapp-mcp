import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, mkdir, writeFile, symlink, rm, realpath, copyFile } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { request } from 'node:http';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import { createPreview } from '../scripts/preview.mjs';

function get(port, url, method = 'GET') {
  return new Promise((resolve, reject) => {
    const req = request({ hostname: '127.0.0.1', port, path: url, method }, res => {
      let body = ''; res.setEncoding('utf8');
      res.on('data', data => { body += data; });
      res.on('end', () => resolve({ code: res.statusCode, body, headers: res.headers }));
    });
    req.on('error', reject); req.end();
  });
}

test('serves the exact generated search chunk but rejects other at-sign paths', async t => {
  const root = await realpath(await mkdtemp(path.join(os.tmpdir(), 'wa-search-preview-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  const distRoot = path.join(root, 'dist');
  await mkdir(path.join(distRoot, 'assets/chunks'), { recursive: true });
  for (const name of ['@localSearchIndexroot.abcdefgh.js', '@private.abcdefgh.js']) await writeFile(path.join(distRoot, 'assets/chunks', name), 'SEARCH');
  const server = await createPreview({ distRoot, port: 0, env: {} });
  t.after(() => new Promise(resolve => server.close(resolve)));
  const port = server.address().port;
  assert.equal((await get(port, '/my-whatsapp-mcp/assets/chunks/@localSearchIndexroot.abcdefgh.js')).code, 200);
  for (const suffix of ['@private.abcdefgh.js', '%40localSearchIndexroot.abcdefgh.js', '@localSearchIndexroot...js']) assert.equal((await get(port, `/my-whatsapp-mcp/assets/chunks/${suffix}`)).code, 404);
});

test('literal loopback serves only direct built output below the exact repository base', async t => {
  const root = await realpath(await mkdtemp(path.join(os.tmpdir(), 'wa-static-preview-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  const distRoot = path.join(root, 'dist');
  for (const directory of ['install', 'assets', 'markdown/install', '.vitepress']) await mkdir(path.join(distRoot, directory), { recursive: true });
  for (const [file, text] of [['index.html', 'HOME'], ['install/docker-compose.html', 'GUIDE'], ['assets/app.js', 'ASSET'], ['vp-icons.css', 'ICONS'], ['markdown/install/docker-compose.md', 'MARKDOWN'], ['llms-full.txt', 'BUNDLE'], ['.vitepress/private.html', 'PRIVATE'], ['inventory.json', 'PRIVATE'], ['private.html', 'PRIVATE']]) await writeFile(path.join(distRoot, file), text);
  await writeFile(path.join(root, 'secret.md'), 'SECRET');
  await symlink(path.join(root, 'secret.md'), path.join(distRoot, 'assets/escape.js'));
  await symlink(root, path.join(distRoot, 'assets/linked'));
  const server = await createPreview({ distRoot, port: 0, env: {} });
  t.after(() => new Promise(resolve => server.close(resolve)));
  assert.equal(server.address().address, '127.0.0.1');
  const port = server.address().port;
  for (const [url, expected] of [['/', 'HOME'], ['/index.html', 'HOME'], ['/install/docker-compose.html', 'GUIDE'], ['/assets/app.js', 'ASSET'], ['/vp-icons.css', 'ICONS'], ['/markdown/install/docker-compose.md', 'MARKDOWN'], ['/llms-full.txt', 'BUNDLE']]) {
    const response = await get(port, `/my-whatsapp-mcp${url}`);
    assert.equal(response.code, 200, url); assert.equal(response.body, expected);
    assert.equal(response.headers['x-content-type-options'], 'nosniff');
  }
  assert.equal((await get(port, '/my-whatsapp-mcp/install/docker-compose.html', 'HEAD')).body, '');
  for (const url of ['/', '/my-whatsapp-mcp', '/other/index.html', '/my-whatsapp-mcp/install/docker-compose', '/my-whatsapp-mcp/install/', '/my-whatsapp-mcp/../secret.md', '/my-whatsapp-mcp/%2e%2e/secret.md', '/my-whatsapp-mcp/assets%2fescape.js', '/my-whatsapp-mcp/assets/escape.js', '/my-whatsapp-mcp/assets/linked/secret.md', '/my-whatsapp-mcp/.vitepress/private.html', '/my-whatsapp-mcp/private.html', '/my-whatsapp-mcp/inventory.json', '/my-whatsapp-mcp/node_modules/a.js']) {
    const response = await get(port, url); assert.equal(response.code, 404, url); assert.doesNotMatch(response.body, /SECRET|PRIVATE|HOME/);
  }
  const post = await get(port, '/my-whatsapp-mcp/index.html', 'POST');
  assert.equal(post.code, 405); assert.equal(post.headers.allow, 'GET, HEAD');
});

test('symlink roots and ancestors are refused before listening', async t => {
  const root = await realpath(await mkdtemp(path.join(os.tmpdir(), 'wa-static-symlink-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(path.join(root, 'real/dist'), { recursive: true });
  await symlink(path.join(root, 'real'), path.join(root, 'linked'));
  await assert.rejects(createPreview({ distRoot: path.join(root, 'linked/dist'), port: 0, env: {} }), /[Ss]ymlink/);
});

test('actual npm preview command binds loopback and rejects host overrides', { timeout: 15_000 }, async t => {
  const source = fileURLToPath(new URL('../', import.meta.url));
  const cwd = await realpath(await mkdtemp(path.join(os.tmpdir(), 'wa-preview-command-')));
  t.after(() => rm(cwd, { recursive: true, force: true }));
  await mkdir(path.join(cwd, 'scripts'));
  await mkdir(path.join(cwd, 'dist/install'), { recursive: true });
  // Run the unmodified package preview script in an isolated fixture so npm test
  // remains valid before a site build and never changes the actual dist tree.
  for (const file of ['package.json', 'public-pages.mjs', 'scripts/preview.mjs', 'scripts/site.mjs', 'scripts/urls.mjs']) await copyFile(path.join(source, file), path.join(cwd, file));
  await writeFile(path.join(cwd, 'dist/install/docker-compose.html'), 'CLI GUIDE');
  const npmCLI = process.env.npm_execpath ?? path.resolve(path.dirname(process.execPath), '../lib/node_modules/npm/bin/npm-cli.js');
  for (const args of [['--host', '0.0.0.0'], ['--host', '127.0.0.1'], ['--port', '-1']]) {
    const child = spawn(process.execPath, [npmCLI, 'run', 'preview', '--', ...args], { cwd, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = ''; child.stderr.on('data', chunk => { output += chunk; });
    assert.equal((await once(child, 'close'))[0], 1);
    assert.match(output, /Usage|port/);
  }
  const child = spawn(process.execPath, [npmCLI, 'run', 'preview', '--', '--port', '0'], { cwd, stdio: ['ignore', 'pipe', 'pipe'] });
  t.after(() => { if (child.exitCode === null) child.kill(); });
  let output = '';
  const address = await new Promise((resolve, reject) => {
    child.stdout.on('data', chunk => {
      output += chunk;
      const match = /Static documentation: http:\/\/127\.0\.0\.1:(\d+)\/my-whatsapp-mcp\//.exec(output);
      if (match) resolve(Number(match[1]));
    });
    child.once('exit', code => reject(new Error(`Preview exited ${code}: ${output}`)));
    child.stderr.on('data', chunk => { output += chunk; });
  });
  assert.equal((await get(address, '/my-whatsapp-mcp/install/docker-compose.html')).code, 200);
  assert.equal((await get(address, '/docs/compatibility.json')).code, 404);
  child.kill(); await once(child, 'close');
});
