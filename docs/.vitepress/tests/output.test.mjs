import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { checkOutput } from '../scripts/check-output.mjs';

async function fixture(t) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'docs-output-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const distRoot = path.join(root, 'dist');
  const stagingRoot = path.join(root, '.content');
  const inventory = { pages: [{ source: 'index.md', route: 'index', title: 'Home' }, { source: 'install/nested.md', route: 'install/nested', title: 'Nested' }], downloads: [{ source: 'images/example.svg', route: 'images/example.svg' }] };
  const put = async (name, value) => { const target = path.join(distRoot, name); await mkdir(path.dirname(target), { recursive: true }); await writeFile(target, value); };
  await mkdir(path.join(stagingRoot, 'images'), { recursive: true });
  await writeFile(path.join(stagingRoot, 'inventory.json'), JSON.stringify(inventory));
  await writeFile(path.join(stagingRoot, 'images/example.svg'), '<svg/>');
  await put('images/example.svg', '<svg/>');
  await put('index.html', '<h1 id="home">Home</h1><a href="/my-whatsapp-mcp/install/nested.html#nested">Nested</a>');
  await put('install/nested.html', '<h1 id="nested">Nested</h1><img src="../images/example.svg"><a href="../index.html#home">Home</a>');
  await put('markdown/index.md', '# Home\n\n[Next](https://devshiftteam.github.io/my-whatsapp-mcp/install/nested.html#nested)\n');
  await put('markdown/install/nested.md', '# Nested\n');
  await put('llms.txt', '# Guides\n[Home](https://devshiftteam.github.io/my-whatsapp-mcp/index.html)\n');
  await put('llms-full.txt', '# Home\n\n[Next](https://devshiftteam.github.io/my-whatsapp-mcp/install/nested.html#nested)\n\n# Nested\n');
  await put('hashmap.json', JSON.stringify({ 'index.md': 'abcdefgh', 'install_nested.md': 'ijklmnop' }));
  for (const [name, hash] of [['index', 'abcdefgh'], ['install_nested', 'ijklmnop']]) {
    for (const suffix of ['', '.lean']) await put(`assets/${name}.md.${hash}${suffix}.js`, 'export default {};');
  }
  await put('assets/chunks/@localSearchIndexroot.abcdefgh.js', `const index = ${JSON.stringify(JSON.stringify({ documentCount: 2, documentIds: { 0: '/my-whatsapp-mcp/index.html#home', 1: '/my-whatsapp-mcp/install/nested.html#nested' } }))}; export { index as default };`);
  return { distRoot, stagingRoot, put, check: () => checkOutput({ distRoot, stagingRoot }) };
}

test('accepts a complete nested allowlisted artifact', async t => {
  const f = await fixture(t);
  assert.equal((await f.check()).pages, 2);
});

for (const [name, mutate, error] of [
  ['missing nested asset', f => rm(path.join(f.distRoot, 'images/example.svg')), /Missing.*images\/example.svg/],
  ['missing Markdown export', f => rm(path.join(f.distRoot, 'markdown/install/nested.md')), /Missing.*markdown\/install\/nested.md/],
  ['bad HTML anchor', f => f.put('install/nested.html', '<h1 id="nested">Nested</h1><a href="../index.html#absent">Bad</a>'), /anchor.*absent/i],
  ['bad Markdown reference anchor', f => f.put('llms.txt', '[Bad][ref]\n\n[ref]: https://devshiftteam.github.io/my-whatsapp-mcp/index.html#absent\n'), /anchor.*absent/i],
  ['extra internal HTML page', f => f.put('planning/secret.html', '<h1>Private</h1>'), /Unlisted.*planning\/secret.html/],
  ['unlisted copied asset', f => f.put('assets/internal.png', 'private'), /Unlisted.*internal.png/],
  ['unlisted page chunk', f => f.put('assets/private.md.abcdefgh.js', 'export default {};'), /Unlisted.*private.md/],
  ['private sentinel in search chunk', f => f.put('assets/chunks/@localSearchIndexroot.abcdefgh.js', 'const x = "PRIVATE_DOCS_SENTINEL";'), /private.*sentinel/i],
  ['encoded local path', f => f.put('index.html', '<a href="%252fUsers%252fprivate%252ftoken">Bad</a>'), /private.*path/i],
  ['HTML entity encoded local path', f => f.put('index.html', '<p>&#47;Users&#47;private&#47;token</p>'), /private.*path/i],
  ['excluded security record in search', f => f.put('assets/chunks/@localSearchIndexroot.abcdefgh.js', 'const index = "DEPENDENCY_SECURITY.md";'), /Internal document/i],
  ['credential pattern', f => f.put('llms.txt', 'ghp_abcdefghijklmnopqrstuvwxyz1234567890'), /credential/i],
  ['changed copied asset', f => f.put('images/example.svg', '<svg>unreviewed</svg>'), /differs.*source/i],
  ['escaped deployment prefix', f => f.put('index.html', '<a href="/other/index.html">Bad</a>'), /outside.*base/i],
  ['bundle drift', f => f.put('llms-full.txt', '# Omitted procedure'), /bundle.*differ/i],
  ['missing JS import', f => f.put('assets/index.md.abcdefgh.js', 'import "./missing.js";'), /Missing local target.*missing.js/],
  ['missing CSS image', f => f.put('assets/style.abcdefgh.css', 'body { background: url("./nested/missing.png") }'), /Missing local target.*nested\/missing.png/],
  ['missing CSS image-set string', f => f.put('assets/style.abcdefgh.css', 'body { background: image-set("./missing.png" 1x, "../images/example.svg" 2x) }'), /Missing local target.*missing.png/],
  ['missing prefixed CSS image-set string', f => f.put('assets/style.abcdefgh.css', 'body { background: -webkit-image-set("./missing.png" 1x) }'), /Missing local target.*missing.png/],
  ['missing inline style URL', f => f.put('index.html', '<h1 id="home" style="background:url(./missing.png)">Home</h1>'), /Missing local target.*missing.png/],
  ['missing inline style image-set', f => f.put('index.html', '<h1 id="home" style="background:image-set(&quot;./missing.png&quot; 1x)">Home</h1>'), /Missing local target.*missing.png/],
  ['missing style block import', f => f.put('index.html', '<h1 id="home">Home</h1><style>@import "./missing.css";</style>'), /Missing local target.*missing.css/],
  ['missing style block image-set', f => f.put('install/nested.html', '<h1 id="nested">Nested</h1><style>body { background:image-set("../missing.png" 1x) }</style>'), /Missing local target.*missing.png/],
  ['missing CSS import', f => f.put('assets/style.abcdefgh.css', '@import "./missing.css";'), /Missing local target.*missing.css/],
  ['private search route', f => f.put('assets/chunks/@localSearchIndexroot.abcdefgh.js', `const index = ${JSON.stringify(JSON.stringify({ documentCount: 1, documentIds: { 0: '/my-whatsapp-mcp/private.html' } }))};`), /Missing local target.*private.html/],
  ['incomplete search inventory', f => f.put('assets/chunks/@localSearchIndexroot.abcdefgh.js', `const index = ${JSON.stringify(JSON.stringify({ documentCount: 1, documentIds: { 0: '/my-whatsapp-mcp/index.html#home' } }))};`), /Search inventory/],
]) {
  test(`rejects ${name}`, async t => { const f = await fixture(t); await mutate(f); await assert.rejects(f.check, error); });
}

test('allows only the exact reviewed user path placeholder', async t => {
  const f = await fixture(t);
  await f.put('index.html', '<h1 id="home">Home</h1><code>/Users/YOU/.cloudflared/example.yml</code>');
  await f.check();
  await f.put('index.html', '<h1 id="home">Home</h1><code>/Users/YOU-private/token</code>');
  await assert.rejects(f.check, /Private local path/);
});

test('accepts external and inline CSS resources without treating type strings as URLs', async t => {
  const f = await fixture(t);
  await f.put('assets/style.abcdefgh.css', 'body { background: image-set("../images/example.svg" 1x type("image/svg+xml"), url("../images/example.svg") 2x) }');
  await f.put('index.html', '<h1 id="home" style="background:image-set(&quot;./images/example.svg&quot; 1x type(&quot;image/svg+xml&quot;))">Home</h1><style>@import "./assets/style.abcdefgh.css"; body { background: url("./images/example.svg") }</style>');
  await f.check();
});
