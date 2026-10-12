import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, mkdir, writeFile, readFile, rm, realpath, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { exportPage, markdownURL } from '../scripts/markdown.mjs';
import { exportDocumentation } from '../scripts/export.mjs';
import { prepare } from '../scripts/prepare.mjs';

const baseURL = 'https://devshiftteam.github.io/my-whatsapp-mcp/';
const qualification = { availability: 'planned', status: 'documentation_reviewed', reviewedAt: '2026-10-12', tests: [] };
const pages = [
  { source: 'install/docker-compose.md', route: 'install/docker-compose', title: 'Docker Compose' },
  { source: 'clients/codex.md', route: 'clients/codex', title: 'Codex setup' }
];
const inventory = { pages, downloads: [{ source: 'assets/example.json', route: 'downloads/example.json' }] };
const options = { ...pages[0], baseURL, qualification, inventory };

test('removes YAML frontmatter and emits one title, source URL and a plain setup notice', () => {
  const result = exportPage({ ...options, markdown: '---\ntitle: old\nprivate: omitted\n---\n# Old title\n\nProcedure.\n' });
  assert.equal((result.match(/^# /gm) || []).length, 1);
  assert.match(result, /^# Docker Compose\n/);
  assert.match(result, /Source: <https:\/\/devshiftteam.github.io\/my-whatsapp-mcp\/install\/docker-compose.html>/);
  assert.match(result, /Not available yet/);
  assert.doesNotMatch(result, /documentation_reviewed|reviewedAt|"tests"|Qualification evidence/);
  assert.doesNotMatch(result, /private:|Old title|^---/m);
});

test('retains both OS variants and complete fence bodies', () => {
  const input = '::: code-group\n\n```sh [macOS/Linux]\ndocker compose up -d\n```\n\n```powershell [Windows]\ndocker compose up -d\n```\n\n:::\n';
  const result = exportPage({ source: 'install/docker-compose.md', route: 'install/docker-compose',
    title: 'Docker Compose', markdown: input, baseURL,
    qualification: { availability: 'planned', status: 'documentation_reviewed', reviewedAt: '2026-10-12', tests: [] } });
  assert.match(result, /### macOS\/Linux/);
  assert.match(result, /### Windows/);
  assert.equal((result.match(/docker compose up -d/g) || []).length, 2);
  assert.doesNotMatch(result, /:::|<script|<PageMarkdown/);
  const fence = '~~~~sh\n::: warning\n[local](file:///private/secret)\n<script>literal</script>\n```\n~~~~\n';
  assert.ok(exportPage({ ...options, markdown: fence }).includes(fence));
});

test('converts admonitions and removes only recognized decorative components', () => {
  const result = exportPage({ ...options, markdown: '::: warning Check first\nKeep this instruction.\n\n```sh\necho safe\n```\n:::\n<PageMarkdown />\n' });
  assert.match(result, /> \*\*Check first\*\*/);
  assert.match(result, /Keep this instruction/);
  assert.match(result, /```sh\necho safe\n```/);
  assert.doesNotMatch(result, /:::|PageMarkdown/);
  for (const markdown of ['<Instruction>Do a thing</Instruction>', '<script>alert(1)</script>', '<div>Do a thing</div>', '::: unknown\nDo a thing\n:::', '<PageMarkdown>instructions</PageMarkdown>', 'Text <SecretStep />']) {
    assert.throws(() => exportPage({ ...options, markdown }), /unsupported|unrecognized/i, markdown);
  }
});

test('rewrites relative page, asset and reference links without touching fenced or inline code', () => {
  const markdown = '[Codex setup](../clients/codex.md#setup "Client setup") [here](#setup)\n\n[example](../assets/example.json)\n\n[Reference][client]\n\n[client]: ../clients/codex.md#setup "A title with spaces"\n\n`[code](../private.md)`\n\n```sh\n[code](../private.md)\n```\n';
  const result = exportPage({ ...options, markdown });
  assert.match(result, /\[Codex setup\]\(https:\/\/devshiftteam.github.io\/my-whatsapp-mcp\/clients\/codex.html#setup "Client setup"\)/);
  assert.match(result, /\[here\]\(#setup\)/);
  assert.match(result, /downloads\/example.json/);
  assert.match(result, /\[client\]: https:\/\/devshiftteam.github.io\/my-whatsapp-mcp\/clients\/codex.html#setup/);
  assert.ok(result.includes('`[code](../private.md)`'));
  assert.ok(result.includes('```sh\n[code](../private.md)\n```'));
});

test('preserves external HTTPS queries, anchors, spaces and explicit canonical links', () => {
  const markdown = '[Search](https://example.com/?q=hello&lang=en#results) [Space](<https://example.com/a%20b>) [Title](../clients/codex.md "with spaces")';
  const result = exportPage({ ...options, markdown });
  assert.ok(result.includes('[Search](https://example.com/?q=hello&lang=en#results)'));
  assert.ok(result.includes('[Space](<https://example.com/a%20b>)'));
  assert.match(result, /codex.html "with spaces"/);
  assert.match(exportPage({ ...options, markdown: '[Codex](/my-whatsapp-mcp/clients/codex.html#setup)' }), /codex.html#setup/);
});

test('preserves indented commands and rewrites later headings, multiline references and canonical autolinks', () => {
  const markdown = '    echo indented\n    echo still-indented\n\n# First title\n\n# [Client](../clients/codex.md)\n\n[ref][client]\n\n[client]:\n  ../clients/codex.md#setup\n\n<https://devshiftteam.github.io/my-whatsapp-mcp/clients/codex.md>\n';
  const result = exportPage({ ...options, markdown });
  assert.ok(result.includes('    echo indented\n    echo still-indented\n'));
  assert.match(result, /## \[Client\]\(https:\/\/devshiftteam.github.io\/my-whatsapp-mcp\/clients\/codex.html\)/);
  assert.match(result, /\[client\]:\n  https:\/\/devshiftteam.github.io\/my-whatsapp-mcp\/clients\/codex.html#setup/);
  assert.ok(result.includes('<https://devshiftteam.github.io/my-whatsapp-mcp/clients/codex.html>'));
});

test('rewrites reference destinations through blockquotes and nested lists without changing container prefixes', () => {
  const canonical = `${baseURL}clients/codex.html#setup`;
  for (const [markdown, expected] of [
    ['> [Client][client]\n>\n> [client]: ../clients/codex.md#setup\n', `> [Client][client]\n>\n> [client]: ${canonical}\n`],
    ['> > [Client][client]\n> >\n> > [client]:\n> >   <../clients/codex.md#setup> "Client setup"\n', `> > [Client][client]\n> >\n> > [client]:\n> >   <${canonical}> "Client setup"\n`],
    ['- Parent\n  - [Client][client]\n\n    [client]: ../clients/codex.md#setup\n', `- Parent\n  - [Client][client]\n\n    [client]: ${canonical}\n`],
    ['1. Parent\n   - [Client][client]\n\n     [client]:\n       ../clients/codex.md#setup\n', `1. Parent\n   - [Client][client]\n\n     [client]:\n       ${canonical}\n`],
    ['- Parent\n  > [Client][client]\n  >\n  > [client]: ../clients/codex.md#setup\n', `- Parent\n  > [Client][client]\n  >\n  > [client]: ${canonical}\n`],
    ['> [Client][client]\r\n>\r\n> [client]: ../clients/codex.md#setup\r\n', `> [Client][client]\r\n>\r\n> [client]: ${canonical}\r\n`]
  ]) {
    assert.ok(exportPage({ ...options, markdown }).includes(expected), markdown);
  }
});

test('maps individual inline destinations to original multiline quote and list source offsets', () => {
  const canonical = `${baseURL}clients/codex.html`;
  for (const [markdown, expected] of [
    ['> Follow the client\n> [setup](../clients/codex.md).\n', `> Follow the client\n> [setup](${canonical}).\n`],
    ['> > Follow the client\n> > [setup](../clients/codex.md "Client setup").\n', `> > Follow the client\n> > [setup](${canonical} "Client setup").\n`],
    ['- Parent\n  - Follow the client\n    [setup](../clients/codex.md).\n', `- Parent\n  - Follow the client\n    [setup](${canonical}).\n`],
    ['1. Parent\n   - Follow the client\n     [setup](../clients/codex.md).\n', `1. Parent\n   - Follow the client\n     [setup](${canonical}).\n`],
    ['- Parent\n  > Follow the client\n  > [setup](../clients/codex.md).\n', `- Parent\n  > Follow the client\n  > [setup](${canonical}).\n`],
    ['> First [setup](../clients/codex.md),\n> then [setup](../clients/codex.md#setup).\n', `> First [setup](${canonical}),\n> then [setup](${canonical}#setup).\n`],
    ['- Parent\n\t- Follow the client\n\t  [setup](../clients/codex.md).\n', `- Parent\n\t- Follow the client\n\t  [setup](${canonical}).\n`],
    ['> Follow the client\n[setup](../clients/codex.md).\n', `> Follow the client\n[setup](${canonical}).\n`],
    ['> [setup](\n> <../clients/codex.md>\n> "Client title").\n', `> [setup](\n> <${canonical}>\n> "Client title").\n`],
    ['> Follow the client\r\n> [setup](../clients/codex.md).\r\n', `> Follow the client\r\n> [setup](${canonical}).\r\n`]
  ]) assert.ok(exportPage({ ...options, markdown }).includes(expected), markdown);
});

test('nested source mapping protects fenced/inline code and still rejects unlisted destinations', () => {
  const code = '> ```md\n> [private](../acceptance/private.md)\n> [private]: ../acceptance/private.md\n> ```\n';
  const inline = '- Parent\n  - Literal `[private](../acceptance/private.md)`\n    and `[private]: ../acceptance/private.md`.\n';
  const result = exportPage({ ...options, markdown: code + '\n' + inline });
  assert.ok(result.includes(code));
  assert.ok(result.includes(inline));
  for (const markdown of [
    '> Follow the client\n> [private](../acceptance/private.md).\n',
    '> [private][secret]\n>\n> [secret]: ../acceptance/private.md\n',
    '- Parent\n  - Follow the client\n    [private](../acceptance/private.md).\n',
    '- Parent\n  - [private][secret]\n\n    [secret]: ../acceptance/private.md\n',
    '> Unreferenced definition\n>\n> [secret]: ../acceptance/private.md\n'
  ]) assert.throws(() => exportPage({ ...options, markdown }), /excluded|unlisted/i, markdown);
});

test('rejects excluded, private and unsafe links, including images and references', () => {
  for (const target of ['../acceptance/private.md', '../private.md', 'file:///Users/private/report.md', '/Users/private/report.md', 'C:/private/report.md', 'javascript:alert(1)', 'http://example.com', '../../outside.md', '../clients/codex.md?token=private', 'https://example.com@localhost/private']) {
    assert.throws(() => exportPage({ ...options, markdown: `[Bad](<${target}>)` }), /link|excluded|unsafe/i, target);
  }
  assert.throws(() => exportPage({ ...options, markdown: '![Private](../private.png)' }), /link|excluded/i);
  assert.throws(() => exportPage({ ...options, markdown: '[Bad][x]\n\n[x]: ../acceptance/private.md' }), /link|excluded/i);
});

test('rejects executable and non-YAML frontmatter without evaluating it', () => {
  delete globalThis.__waExportExecuted;
  for (const language of ['javascript', 'js', 'json', 'toml']) {
    assert.throws(() => exportPage({ ...options, markdown: `---${language}\n(globalThis.__waExportExecuted = true, {})\n---\nProcedure.` }), /frontmatter/i);
  }
  assert.equal(globalThis.__waExportExecuted, undefined);
  for (const language of ['yaml', 'yml']) assert.match(exportPage({ ...options, markdown: `---${language}\ntitle: YAML\n---\nProcedure.` }), /Procedure/);
});

test('keeps build evidence out of user exports and rejects staged qualification drift', () => {
  const local = { ...qualification, availability: 'preview', status: 'locally_tested', tests: [{ buildSha: 'a'.repeat(40), platform: 'darwin', architecture: 'arm64', provider: null, client: null, fixture: 'synthetic-search-v1', testedAt: '2026-10-12', outcome: 'passed' }] };
  const result = exportPage({ ...options, qualification: local, markdown: 'Run the procedure.' });
  assert.match(result, /Preview setup/);
  assert.match(result, /still in preview/);
  assert.doesNotMatch(result, /locally_tested|buildSha|synthetic-search-v1|"provider"/);
  assert.throws(() => exportPage({ ...options, markdown: '---\nqualification:\n  availability: preview\n  status: documentation_reviewed\n  reviewedAt: "2026-10-12"\n  tests: []\n---\nProcedure.' }), /qualification differs/);
});

test('constructs explicit Markdown paths and validates route/base inputs', () => {
  assert.equal(markdownURL('install/docker-compose', '/my-whatsapp-mcp/'), '/my-whatsapp-mcp/markdown/install/docker-compose.md');
  assert.equal(markdownURL('index', baseURL), `${baseURL}markdown/index.md`);
  for (const route of ['../private', '/private', 'a.html', 'a%2fb']) assert.throws(() => markdownURL(route, baseURL));
  for (const base of ['https://example.com/?token=x', 'http://example.com/', 'https://user:password@example.com/', '//example.com/', '/a/../b/']) assert.throws(() => markdownURL('index', base));
});

async function fixture(t) {
  const root = await realpath(await mkdtemp(path.join(tmpdir(), 'wa-docs-export-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  const docsRoot = path.join(root, 'docs');
  await mkdir(path.join(docsRoot, '.vitepress'), { recursive: true });
  await mkdir(path.join(docsRoot, 'install'));
  await mkdir(path.join(docsRoot, 'clients'));
  await writeFile(path.join(docsRoot, pages[0].source), '# Install\n\nProcedure version one.\n');
  await writeFile(path.join(docsRoot, pages[1].source), '# Codex\n\nConnect the client.\n');
  await writeFile(path.join(docsRoot, 'secret.md'), 'UNLISTED_SECRET_SENTINEL');
  const stagingRoot = path.join(docsRoot, '.vitepress/.content');
  const distRoot = path.join(docsRoot, '.vitepress/dist');
  const prepareOptions = { docsRoot, stagingRoot, pages, downloads: [], qualification: Object.fromEntries(pages.map(page => [page.route, qualification])) };
  await prepare(prepareOptions);
  return { docsRoot, stagingRoot, distRoot, prepareOptions };
}

test('exports deterministically from inventory and combines exact standalone pages with updated procedure/status', async t => {
  const opts = await fixture(t);
  await writeFile(path.join(opts.stagingRoot, 'unlisted.md'), 'STAGED_SECRET_SENTINEL');
  await exportDocumentation({ ...opts, baseURL });
  const first = await readFile(path.join(opts.distRoot, 'llms-full.txt'), 'utf8');
  const standalone = await Promise.all(pages.map(page => readFile(path.join(opts.distRoot, 'markdown', `${page.route}.md`), 'utf8')));
  assert.equal(first, standalone.join('\n'));
  assert.doesNotMatch(first, /SECRET_SENTINEL/);
  const index = await readFile(path.join(opts.distRoot, 'llms.txt'), 'utf8');
  assert.match(index, /^# My WhatsApp documentation\n/);
  for (const page of pages) {
    assert.ok(index.includes(`${baseURL}${page.route}.html`));
    assert.ok(index.includes(markdownURL(page.route, baseURL)));
  }
  await exportDocumentation({ ...opts, baseURL });
  assert.equal(await readFile(path.join(opts.distRoot, 'llms-full.txt'), 'utf8'), first);
  await writeFile(path.join(opts.docsRoot, pages[0].source), '# Install\n\nProcedure version two.\n');
  opts.prepareOptions.qualification[pages[0].route] = { ...qualification, availability: 'preview', reviewedAt: '2026-10-13' };
  await prepare(opts.prepareOptions);
  await exportDocumentation({ ...opts, baseURL });
  for (const file of ['llms-full.txt', 'markdown/install/docker-compose.md']) {
    const content = await readFile(path.join(opts.distRoot, file), 'utf8');
    assert.match(content, /Procedure version two/);
    assert.match(content, /Preview setup/);
    assert.match(content, /not yet been tested from start to finish/);
    assert.doesNotMatch(content, /documentation_reviewed|reviewedAt|2026-10-13/);
    assert.doesNotMatch(content, /Procedure version one|SECRET_SENTINEL/);
  }
});

test('rejects staged source or output symlinks and validates everything before changing existing exports', async t => {
  const opts = await fixture(t);
  await exportDocumentation({ ...opts, baseURL });
  const previous = await readFile(path.join(opts.distRoot, 'llms-full.txt'), 'utf8');
  const staged = path.join(opts.stagingRoot, `${pages[0].route}.md`);
  await writeFile(staged, '# Unsafe\n\n[private](../acceptance/private.md)');
  await assert.rejects(exportDocumentation({ ...opts, baseURL }), /excluded|link/i);
  assert.equal(await readFile(path.join(opts.distRoot, 'llms-full.txt'), 'utf8'), previous);
  await rm(staged);
  await symlink(path.join(opts.docsRoot, 'secret.md'), staged);
  await assert.rejects(exportDocumentation({ ...opts, baseURL }), /symlink/i);
  await prepare(opts.prepareOptions);
  await rm(path.join(opts.distRoot, 'markdown'), { recursive: true });
  await symlink(opts.docsRoot, path.join(opts.distRoot, 'markdown'));
  await assert.rejects(exportDocumentation({ ...opts, baseURL }), /symlink/i);
  assert.equal(await readFile(path.join(opts.docsRoot, 'secret.md'), 'utf8'), 'UNLISTED_SECRET_SENTINEL');
});

test('maps table links to exact source offsets and preserves escaped pipes, code and alignment', () => {
  const markdown = '| [Header](../clients/codex.md) | Kind |\r\n| :--- | ---: |\r\n| escaped \\| [Codex](../clients/codex.md#setup "title") | `[literal](../private.md)` |\r\n| [external](https://example.com/docs?a=1) | code `a\\|b` |\r\n| [again](../clients/codex.md) | [Reference][client] |\r\n\r\n[client]: ../clients/codex.md#setup\r\n';
  const expected = markdown.replaceAll('../clients/codex.md', `${baseURL}clients/codex.html`);
  assert.ok(exportPage({ ...options, markdown }).endsWith(expected));
});

test('maps tables within blockquotes and lists including duplicate cells and missing final cells', () => {
  for (const prefix of ['> ', '  ']) {
    const leader = prefix === '  ' ? '- Table:\n\n' : '';
    const markdown = leader + ['| Link | Link |', '| --- | --- |', '| [same](../clients/codex.md) | [same](../clients/codex.md) |', '| [short](../clients/codex.md) |'].map(line => prefix + line).join('\n') + '\n';
    assert.ok(exportPage({ ...options, markdown }).endsWith(markdown.replaceAll('../clients/codex.md', `${baseURL}clients/codex.html`)));
  }
});

test('table destinations cannot bypass inventory denial or unsafe-link checks', () => {
  for (const destination of ['../acceptance/private.md', 'file:///private/secret', 'https://user:password@example.com/', `${baseURL}secret.html`]) {
    assert.throws(() => exportPage({ ...options, markdown: `| Link |\n| --- |\n| [private](${destination}) |\n` }), /excluded|unsafe|private/i);
    assert.throws(() => exportPage({ ...options, markdown: `| Link |\n| --- |\n| [private][ref] |\n\n[ref]: ${destination}\n` }), /excluded|unsafe|private/i);
  }
});

test('exports only exact reviewed download bytes without broad asset copying',async t=>{
 const opts=await fixture(t);
 await mkdir(path.join(opts.docsRoot,'assets'));
 const bytes=Buffer.from([137,80,78,71,0,255]);
 await writeFile(path.join(opts.docsRoot,'assets','logo.png'),bytes);
 await writeFile(path.join(opts.docsRoot,'assets','private-report.txt'),'PRIVATE_DOCS_SENTINEL');
 await prepare({...opts.prepareOptions,downloads:[{source:'assets/logo.png',route:'assets/logo.png'}]});
 await exportDocumentation({...opts,baseURL});
 assert.deepEqual(await readFile(path.join(opts.distRoot,'assets','logo.png')),bytes);
 await assert.rejects(readFile(path.join(opts.distRoot,'assets','private-report.txt')),/ENOENT/);
});
