import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, readdir, symlink, rm, realpath } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { prepare, validateInventory, validateQualification, validateQualifications } from '../scripts/prepare.mjs';

const page = { source: 'index.md', route: 'index', title: 'WA Indexer', section: 'Start' };
const reviewed = { availability: 'planned', status: 'documentation_reviewed', reviewedAt: '2026-10-12', tests: [] };
const localTest = {
  buildSha: 'a'.repeat(40), platform: 'darwin', architecture: 'arm64',
  provider: null, client: null, fixture: 'synthetic-search-v1', testedAt: '2026-10-12', outcome: 'passed'
};

async function fixture(t) {
  const root = await realpath(await mkdtemp(path.join(tmpdir(), 'wa-docs-prepare-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  const docsRoot = path.join(root, 'docs');
  await mkdir(path.join(docsRoot, '.vitepress'), { recursive: true });
  await writeFile(path.join(docsRoot, 'index.md'), '---\ntitle: WA Indexer\n---\n# Private archive\n\nCanonical instructions.\n');
  const stagingRoot = path.join(docsRoot, '.vitepress', '.content');
  return { root, docsRoot, stagingRoot, pages: [page], downloads: [], qualification: { index: reviewed } };
}

test('the docs test runtime is Node 24', () => assert.equal(process.versions.node.split('.')[0], '24'));

test('rejects traversal and duplicate public routes', () => {
  assert.throws(() => validateInventory([{ source: '../private.md', route: 'x' }]));
  assert.throws(() => validateInventory([{ source: 'a.md', route: 'same' }, { source: 'b.md', route: 'same' }]));
});

test('rejects absolute, encoded, extension-bearing and ambiguous page routes', () => {
  for (const route of ['/x', '../x', 'x/../y', 'x.html', 'x.md', 'x%2fy', 'x\\y', 'x//y', 'x/', '.hidden', 'X?token=s']) {
    assert.throws(() => validateInventory([{ ...page, route }]), route);
  }
  for (const source of ['/tmp/a.md', 'x/../a.md', 'x\\a.md', '.vitepress/a.md', 'a.html']) {
    assert.throws(() => validateInventory([{ ...page, source }]), source);
  }
  assert.throws(() => validateInventory([page, { ...page, route: 'other' }]));
});

test('rejects duplicate destinations across pages and downloads and unknown download extensions', () => {
  assert.throws(() => validateInventory([page], [{ source: 'logo.svg', route: 'index.md' }]));
  assert.throws(() => validateInventory([], [{ source: 'run.exe', route: 'assets/run.exe' }]));
  assert.throws(() => validateInventory([], [{ source: 'a.svg', route: 'assets/a.png' }]));
  assert.throws(() => validateInventory([], [{ source: 'a.svg', route: 'inventory.json' }]));
});

test('rejects case-insensitive destination collisions and file/ancestor conflicts', () => {
  assert.throws(() => validateInventory([page, { ...page, source: 'other.md', route: 'INDEX' }]));
  assert.throws(() => validateInventory([], [{ source: 'a.svg', route: 'assets/A.svg' }, { source: 'b.svg', route: 'assets/a.svg' }]));
  assert.throws(() => validateInventory([], [{ source: 'a.json', route: 'inventory.json' }]));
  assert.throws(() => validateInventory([], [{ source: 'a.svg', route: 'assets/a.svg' }, { source: 'b.svg', route: 'assets/a.svg/b.svg' }]));
});

test('stages only listed pages and exact assets, with reviewed metadata and canonical prose', async (t) => {
  const opts = await fixture(t);
  await mkdir(path.join(opts.docsRoot, 'acceptance'));
  await mkdir(path.join(opts.docsRoot, 'superpowers'));
  await mkdir(path.join(opts.docsRoot, 'public'));
  await writeFile(path.join(opts.docsRoot, 'acceptance/private.md'), 'PRIVATE_ACCEPTANCE');
  await writeFile(path.join(opts.docsRoot, 'superpowers/plan.md'), 'INTERNAL_PLAN');
  await writeFile(path.join(opts.docsRoot, 'public/private.txt'), 'PRIVATE_ASSET');
  await writeFile(path.join(opts.docsRoot, 'logo.svg'), '<svg xmlns="http://www.w3.org/2000/svg"></svg>');
  opts.downloads = [{ source: 'logo.svg', route: 'assets/logo.svg' }];
  const original = await readFile(path.join(opts.docsRoot, 'index.md'), 'utf8');
  await prepare(opts);
  assert.deepEqual((await readdir(opts.stagingRoot)).sort(), ['assets', 'index.md', 'inventory.json']);
  const staged = await readFile(path.join(opts.stagingRoot, 'index.md'), 'utf8');
  assert.match(staged, /status: documentation_reviewed/);
  assert.match(staged, /availability: planned/);
  assert.ok(staged.endsWith('# Private archive\n\nCanonical instructions.\n'));
  assert.equal(await readFile(path.join(opts.docsRoot, 'index.md'), 'utf8'), original);
  const inventory = JSON.parse(await readFile(path.join(opts.stagingRoot, 'inventory.json'), 'utf8'));
  assert.deepEqual(inventory.pages, [{ ...page, qualification: reviewed }]);
  assert.deepEqual(inventory.downloads, opts.downloads);
  await writeFile(path.join(opts.docsRoot, 'acceptance/another.md'), 'NEW_PRIVATE_DOCUMENT');
  await prepare(opts);
  assert.equal(await readFile(path.join(opts.stagingRoot, 'index.md'), 'utf8'), staged);
});

test('recreates staging without stale files', async (t) => {
  const opts = await fixture(t);
  await mkdir(opts.stagingRoot);
  await writeFile(path.join(opts.stagingRoot, 'stale.md'), 'stale');
  await prepare(opts);
  assert.deepEqual((await readdir(opts.stagingRoot)).sort(), ['index.md', 'inventory.json']);
});

test('rejects a source symlink, including links within docs', async (t) => {
  const opts = await fixture(t);
  await writeFile(path.join(opts.root, 'outside.md'), 'EXTERNAL_SECRET');
  for (const target of [path.join(opts.root, 'outside.md'), path.join(opts.docsRoot, 'index.md')]) {
    const source = 'linked.md';
    await symlink(target, path.join(opts.docsRoot, source));
    await assert.rejects(prepare({ ...opts, pages: [{ ...page, source }] }), /symlink/i);
    await rm(path.join(opts.docsRoot, source));
  }
});

test('rejects symlinked source ancestors and nonregular sources', async (t) => {
  const opts = await fixture(t);
  await mkdir(path.join(opts.root, 'outside'));
  await writeFile(path.join(opts.root, 'outside', 'nested.md'), 'EXTERNAL_SECRET');
  await symlink(path.join(opts.root, 'outside'), path.join(opts.docsRoot, 'linked'));
  await assert.rejects(prepare({ ...opts, pages: [{ ...page, source: 'linked/nested.md' }] }), /symlink/i);
  await mkdir(path.join(opts.docsRoot, 'directory.md'));
  await assert.rejects(prepare({ ...opts, pages: [{ ...page, source: 'directory.md' }] }), /regular file/i);
});

test('rejects unsafe deletion destinations while preserving sentinels', async (t) => {
  const opts = await fixture(t);
  const arbitrary = path.join(opts.root, 'arbitrary');
  await mkdir(arbitrary);
  for (const target of [opts.root, opts.docsRoot, path.dirname(opts.stagingRoot), arbitrary]) {
    const sentinel = path.join(target, 'sentinel.txt');
    await writeFile(sentinel, 'KEEP');
    await assert.rejects(prepare({ ...opts, stagingRoot: target }), /staging/i);
    assert.equal(await readFile(sentinel, 'utf8'), 'KEEP');
  }
  await assert.rejects(prepare({ ...opts, stagingRoot: `${opts.docsRoot}/.vitepress/x/../.content` }), /staging/i);
});

test('rejects symlinked staging destination and destination ancestors before deletion', async (t) => {
  const opts = await fixture(t);
  const outside = path.join(opts.root, 'outside');
  await mkdir(outside);
  await writeFile(path.join(outside, 'sentinel.txt'), 'KEEP');
  await symlink(outside, opts.stagingRoot);
  await assert.rejects(prepare(opts), /symlink/i);
  assert.equal(await readFile(path.join(outside, 'sentinel.txt'), 'utf8'), 'KEEP');
  await rm(opts.stagingRoot);
  await rm(path.dirname(opts.stagingRoot), { recursive: true });
  await symlink(outside, path.dirname(opts.stagingRoot));
  await assert.rejects(prepare(opts), /symlink/i);
  assert.equal(await readFile(path.join(outside, 'sentinel.txt'), 'utf8'), 'KEEP');
});

test('rejects a symlinked docs root and its ancestors', async (t) => {
  const opts = await fixture(t);
  const alias = path.join(opts.root, 'alias');
  await symlink(opts.docsRoot, alias);
  await assert.rejects(prepare({ ...opts, docsRoot: alias, stagingRoot: path.join(alias, '.vitepress/.content') }), /symlink/i);
  const parentAlias = path.join(opts.root, 'parent-alias');
  await symlink(opts.root, parentAlias);
  const docsRoot = path.join(parentAlias, 'docs');
  await assert.rejects(prepare({ ...opts, docsRoot, stagingRoot: path.join(docsRoot, '.vitepress/.content') }), /symlink/i);
});

test('validates all inputs before replacing previously generated staging', async (t) => {
  const opts = await fixture(t);
  await mkdir(opts.stagingRoot);
  await writeFile(path.join(opts.stagingRoot, 'sentinel.txt'), 'KEEP');
  await assert.rejects(prepare({ ...opts, qualification: { index: { ...reviewed, status: 'verified' } } }));
  await assert.rejects(prepare({ ...opts, pages: [{ ...page, source: 'missing.md' }] }));
  assert.equal(await readFile(path.join(opts.stagingRoot, 'sentinel.txt'), 'utf8'), 'KEEP');
});

test('allows documentation review and a locally tested build without client acceptance', () => {
  assert.deepEqual(validateQualification(reviewed), reviewed);
  const local = { ...reviewed, availability: 'preview', status: 'locally_tested', tests: [localTest] };
  assert.deepEqual(validateQualification(local), local);
});

test('end-to-end verification requires complete passing provider and client records', () => {
  for (const tests of [[], [localTest], [{ ...localTest, outcome: 'not_run' }]]) {
    assert.throws(() => validateQualification({ ...reviewed, availability: 'available', status: 'end_to_end_verified', tests }));
  }
  const complete = {
    ...localTest,
    provider: { name: 'cloudflare', version: '2026.10.0', status: 'passed' },
    client: { name: 'codex', version: '1.0.0', status: 'passed' },
    remoteProof: {reportDigest:'sha256:'+'a'.repeat(64),sdkVersion:'1.104.1',helperVersion:'2026.10.0',identity:'passed',reconnect:'passed',restart:'passed',recreate:'passed',upgrade:'passed',clientTools:'passed',authScopeRevocation:'passed'}
  };
  const verified = { ...reviewed, availability: 'available', status: 'end_to_end_verified', tests: [complete] };
  assert.deepEqual(validateQualification(verified), verified);
  assert.throws(() => validateQualification({ ...verified, tests: [{ ...complete, client: { ...complete.client, status: 'failed' } }] }));
});

test('rejects arbitrary qualification, provider, client, and outcome statuses', () => {
  for (const change of [{ status: 'verified' }, { availability: 'ready' }, { reviewedAt: '2026-02-30' }, { unexpected: 'metadata' }]) {
    assert.throws(() => validateQualification({ ...reviewed, ...change }));
  }
  for (const change of [
    { outcome: 'works' }, { buildSha: 'abc123' }, { buildSha: 'g'.repeat(40) },
    { provider: { name: 'ngrok', version: '1.0', status: 'online' } },
    { provider: { name: 'anything', version: '1.0', status: 'passed' } },
    { client: { name: 'codex', version: '1.0', status: 'ready' } },
    { client: { name: 'anything', version: '1.0', status: 'passed' } }
  ]) {
    assert.throws(() => validateQualification({ ...reviewed, status: 'locally_tested', tests: [{ ...localTest, ...change }] }));
  }
});

test('rejects secret-bearing URL metadata, absolute local paths, and missing test evidence', () => {
  for (const fixture of ['https://user:secret@example.com', 'https://example.com?token=secret', 'https://example.com/#secret', '/Users/private/report', 'C:\\private\\report', 'Bearer secret-value']) {
    assert.throws(() => validateQualification({ ...reviewed, status: 'locally_tested', tests: [{ ...localTest, fixture }] }), fixture);
  }
  assert.throws(() => validateQualification({ ...reviewed, status: 'locally_tested', tests: [] }));
  for (const key of ['buildSha', 'platform', 'architecture', 'fixture', 'testedAt', 'outcome', 'provider', 'client']) {
    const incomplete = { ...localTest };
    delete incomplete[key];
    assert.throws(() => validateQualification({ ...reviewed, status: 'locally_tested', tests: [incomplete] }), key);
  }
});

test('requires exactly one qualification per public page', async (t) => {
  const opts = await fixture(t);
  await assert.rejects(prepare({ ...opts, qualification: {} }), /qualification/i);
  await assert.rejects(prepare({ ...opts, qualification: { index: reviewed, hidden: reviewed } }), /qualification/i);
});

test('rejects executable frontmatter without evaluating it or replacing staging', async (t) => {
  const opts = await fixture(t);
  await mkdir(opts.stagingRoot);
  await writeFile(path.join(opts.stagingRoot, 'sentinel.txt'), 'KEEP');
  for (const language of ['javascript', 'js']) {
    await writeFile(path.join(opts.docsRoot, 'index.md'), `---${language}\n({title: 'Unsafe'})\n---\n# Content\n`);
    await assert.rejects(prepare(opts), /Executable frontmatter/i);
    assert.equal(await readFile(path.join(opts.stagingRoot, 'sentinel.txt'), 'utf8'), 'KEEP');
  }
});

import { pages as currentPages } from '../public-pages.mjs';
test('separate remote authentication adds exactly the reviewed phone guide',()=>{
 assert.equal(currentPages.length,36);
 assert.deepEqual(currentPages.filter(p=>p.route==='remote/phone-access').map(p=>p.source),['remote/phone-access.md']);
});

test('verified remote summaries require digest-bound identity, restart and client proof',()=>{
 const test={...localTest,provider:{name:'ngrok',version:'2.2.0',status:'passed'},client:{name:'codex',version:'1.0.0',status:'passed'}};
 const record={...reviewed,status:'end_to_end_verified',tests:[test]};
 assert.throws(()=>validateQualification(record));
 test.remoteProof={reportDigest:'sha256:'+'a'.repeat(64),sdkVersion:'2.2.0',helperVersion:'2026.10.0',identity:'passed',reconnect:'passed',restart:'passed',recreate:'passed',upgrade:'passed',clientTools:'passed',authScopeRevocation:'passed'};
 validateQualification(record);
 for(const key of Object.keys(test.remoteProof)){const copy=structuredClone(record);delete copy.tests[0].remoteProof[key];assert.throws(()=>validateQualification(copy),key);}
});

const fullRemoteProof=()=>({reportDigest:'sha256:'+'a'.repeat(64),sdkVersion:'1.104.1',helperVersion:'2026.10.0',identity:'passed',reconnect:'passed',restart:'passed',recreate:'passed',upgrade:'passed',clientTools:'passed',authScopeRevocation:'passed'});
const verifiedRow=(provider='local')=>({...localTest,provider:{name:provider,version:'synthetic-v1',status:'passed'},client:{name:'browser',version:'synthetic-v1',status:'passed'}});
const routeRecord=row=>({...reviewed,status:'end_to_end_verified',tests:[row]});
const validateRoute=(route,row)=>validateQualifications({[route]:routeRecord(row)},[{...page,route}]);
for(const route of ['remote/tailscale-serve','remote/phone-access','remote/index'])test(`${route} rejects local browser PASS with or without remote proof`,()=>{
 assert.throws(()=>validateRoute(route,verifiedRow()),route);
 assert.throws(()=>validateRoute(route,{...verifiedRow(),remoteProof:fullRemoteProof()}),route);
});
test('a genuinely local page still permits complete local-only verification',()=>validateRoute('install/docker',verifiedRow()));
test('verified remote routes require matching providers and actual reachability evidence',()=>{
 const privateRow={...verifiedRow('tailscale_serve'),remoteProof:{...fullRemoteProof(),enrolledDevice:'passed',unenrolledDevice:'passed'}};
 validateRoute('remote/tailscale-serve',privateRow);validateRoute('remote/index',privateRow);
 for(const key of ['enrolledDevice','unenrolledDevice']){const row=structuredClone(privateRow);delete row.remoteProof[key];assert.throws(()=>validateRoute('remote/tailscale-serve',row),key);}
 const publicRow={...verifiedRow('cloudflare'),remoteProof:{...fullRemoteProof(),externalDevice:'passed'}};
 validateRoute('remote/cloudflare',publicRow);assert.throws(()=>validateRoute('remote/tailscale-serve',publicRow));
 delete publicRow.remoteProof.externalDevice;assert.throws(()=>validateRoute('remote/cloudflare',publicRow));
});
test('verified phone route requires physical phone checks and observed ngrok interstitial',()=>{
 const row={...verifiedRow('ngrok'),remoteProof:{...fullRemoteProof(),externalDevice:'passed',phoneLogin:'passed',phoneSearch:'passed',phoneLogoutDenied:'passed',phoneMCPCredentialRejected:'passed',ngrokInterstitial:'passed'}};
 validateRoute('remote/phone-access',row);
 for(const key of ['phoneLogin','phoneSearch','phoneLogoutDenied','phoneMCPCredentialRejected','ngrokInterstitial']){const copy=structuredClone(row);delete copy.remoteProof[key];assert.throws(()=>validateRoute('remote/phone-access',copy),key);}
 const privateRow={...verifiedRow('tailscale_serve'),remoteProof:{...row.remoteProof,enrolledDevice:'passed',unenrolledDevice:'passed'}};delete privateRow.remoteProof.externalDevice;delete privateRow.remoteProof.ngrokInterstitial;validateRoute('remote/phone-access',privateRow);
});

test('public integration inventory retains every main feature guide and excludes private plans',()=>{
 const required=['setup.md','mcp.md','attachments.md','clients/outbound-messaging.md','stats.md','transcription.md','native-models.md','business-messaging.md','development.md','roadmap.md','alternatives.md','user-guide.md','clients/remote-access.md','clients/codex.md','clients/claude-code.md','clients/claude-connectors.md','clients/composio.md'];
 for(const source of required)assert.ok(currentPages.some(page=>page.source===source),source);
 assert.equal(new Set(currentPages.map(page=>page.source)).size,36);
 assert.equal(currentPages.some(page=>/superpowers|evidence|task-\d|assets\/.*prompt/.test(page.source)),false);
});
