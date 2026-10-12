import { lstat, realpath, readFile, mkdir, writeFile, rm } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import matter from 'gray-matter';
import { pages as publicPages, downloads as publicDownloads } from '../public-pages.mjs';

const pageExtensions = new Set(['.md']);
const downloadExtensions = new Set(['.svg', '.png', '.jpg', '.jpeg', '.webp', '.ico', '.pdf', '.txt', '.json', '.yaml', '.yml', '.zip']);
const availabilityValues = new Set(['planned', 'preview', 'available']);
const qualificationStatuses = new Set(['documentation_reviewed', 'locally_tested', 'end_to_end_verified']);
const results = new Set(['not_run', 'passed', 'failed']);
const providerNames = new Set(['cloudflare', 'ngrok', 'tailscale_serve', 'tailscale_funnel', 'local']);
const clientNames = new Set(['codex', 'claude_code', 'claude_connector', 'composio', 'browser']);
const platforms = new Set(['darwin', 'linux', 'windows']);
const architectures = new Set(['arm64', 'amd64']);
const frontmatterOptions = {
  engines: {
    javascript: { parse() { throw new Error('Executable frontmatter is forbidden'); } }
  }
};

function object(value, label, keys) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} must be an object`);
  for (const key of Object.keys(value)) if (!keys.includes(key)) throw new Error(`Unknown ${label} field: ${key}`);
}

function text(value, label) {
  if (typeof value !== 'string' || !value.trim() || value.length > 240 || /[\x00-\x1f\x7f]/.test(value)) {
    throw new Error(`${label} must be a short, nonempty public string`);
  }
  if (/^(?:[/\\]|[A-Za-z]:[/\\]|~[/\\])/.test(value) || /(?:Bearer\s|(?:token|password|secret|api[_-]?key)\s*[=:])/i.test(value)) {
    throw new Error(`${label} contains a private path or credential`);
  }
  if (value.includes('://')) {
    let url;
    try { url = new URL(value); } catch { throw new Error(`${label} contains a malformed URL`); }
    if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash) {
      throw new Error(`${label} contains unsafe URL metadata`);
    }
  }
}

function enumeration(value, allowed, label) {
  if (!allowed.has(value)) throw new Error(`Invalid ${label}`);
}

function date(value, label) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value)) throw new Error(`Invalid ${label}`);
  const parsed = new Date(`${value}T00:00:00.000Z`);
  if (Number.isNaN(parsed.valueOf()) || parsed.toISOString().slice(0, 10) !== value) throw new Error(`Invalid ${label}`);
}

function participant(value, names, label) {
  if (value === null) return;
  object(value, label, ['name', 'version', 'status']);
  enumeration(value.name, names, `${label} name`);
  text(value.version, `${label} version`);
  enumeration(value.status, results, `${label} status`);
}

/** Validate a public qualification record. Review is independent of runtime acceptance. */
export function validateQualification(record, route = '') {
  const remoteRoute = route.startsWith('remote/');
  const routeProvider = {
    'remote/tailscale-serve': 'tailscale_serve', 'remote/tailscale-funnel': 'tailscale_funnel',
    'remote/ngrok': 'ngrok', 'remote/cloudflare': 'cloudflare'
  }[route];
  object(record, 'qualification', ['availability', 'status', 'reviewedAt', 'tests']);
  enumeration(record.availability, availabilityValues, 'availability');
  enumeration(record.status, qualificationStatuses, 'qualification status');
  date(record.reviewedAt, 'reviewedAt');
  if (!Array.isArray(record.tests)) throw new Error('Qualification tests must be an array');
  for (const test of record.tests) {
    object(test, 'test', ['buildSha', 'platform', 'architecture', 'provider', 'client', 'fixture', 'testedAt', 'outcome', 'remoteProof']);
    if (typeof test.buildSha !== 'string' || !/^[0-9a-f]{40}$/.test(test.buildSha)) throw new Error('Invalid immutable build SHA');
    enumeration(test.platform, platforms, 'platform');
    enumeration(test.architecture, architectures, 'architecture');
    participant(test.provider, providerNames, 'provider');
    participant(test.client, clientNames, 'client');
    text(test.fixture, 'fixture');
    date(test.testedAt, 'testedAt');
    enumeration(test.outcome, results, 'test outcome');
    if (test.remoteProof !== undefined) {
      const keys = ['reportDigest', 'sdkVersion', 'helperVersion', 'identity', 'reconnect', 'restart', 'recreate', 'upgrade', 'clientTools', 'authScopeRevocation'];
      const deviceKeys = ['enrolledDevice', 'unenrolledDevice', 'externalDevice', 'phoneLogin', 'phoneSearch', 'phoneLogoutDenied', 'phoneMCPCredentialRejected', 'ngrokInterstitial'];
      object(test.remoteProof, 'remote proof', [...keys, ...deviceKeys]);
      if (typeof test.remoteProof.reportDigest !== 'string' || !/^sha256:[0-9a-f]{64}$/.test(test.remoteProof.reportDigest)) throw new Error('Invalid acceptance report digest');
      text(test.remoteProof.sdkVersion, 'SDK version');
      text(test.remoteProof.helperVersion, 'helper version');
      for (const key of keys.slice(3)) if (test.remoteProof[key] !== 'passed') throw new Error('Remote proof requires complete identity, restart and client results');
      for (const key of deviceKeys) if (Object.hasOwn(test.remoteProof, key) && test.remoteProof[key] !== 'passed') throw new Error('Remote device proof must be passed');
    }
    if (record.status === 'end_to_end_verified' && (remoteRoute || test.provider && test.provider.name !== 'local') && !test.remoteProof) {
      throw new Error('Verified remote summary requires acceptance identity, restart and client proof');
    }
    if (record.status === 'end_to_end_verified' && remoteRoute) {
      if (!test.provider || test.provider.name === 'local' || routeProvider && test.provider.name !== routeProvider) {
        throw new Error('Verified remote page requires its non-local provider evidence');
      }
      const deviceChecks = test.provider.name === 'tailscale_serve' ? ['enrolledDevice', 'unenrolledDevice'] : ['externalDevice'];
      if (route === 'remote/phone-access') {
        deviceChecks.push('phoneLogin', 'phoneSearch', 'phoneLogoutDenied', 'phoneMCPCredentialRejected');
        if (test.provider.name === 'ngrok') deviceChecks.push('ngrokInterstitial');
      }
      for (const key of deviceChecks) if (test.remoteProof[key] !== 'passed') throw new Error('Verified remote page requires provider and device proof');
    }
  }
  if (record.status === 'locally_tested' && !record.tests.some(test => test.outcome === 'passed')) {
    throw new Error('Locally tested requires a passing build test');
  }
  if (record.status === 'end_to_end_verified' && (
    !record.tests.length || record.tests.some(test =>
      test.outcome !== 'passed' || !test.provider || test.provider.status !== 'passed' || !test.client || test.client.status !== 'passed'
    )
  )) throw new Error('End-to-end verified requires full passing provider and client records');
  return record;
}

function relativeName(value, label) {
  if (typeof value !== 'string' || !value || value.includes('\\') || value.includes('%') || path.posix.isAbsolute(value)) {
    throw new Error(`Invalid ${label}`);
  }
  const segments = value.split('/');
  if (segments.some(segment => !/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(segment) || segment === '..' || segment === '.') || segments.includes('.vitepress')) {
    throw new Error(`Invalid ${label}`);
  }
}

/** Validate exact sources and unique destination paths without touching the filesystem. */
export function validateInventory(pages, downloads = []) {
  if (!Array.isArray(pages) || !Array.isArray(downloads)) throw new Error('Inventory entries must be arrays');
  const sources = new Set();
  const destinations = new Set(['inventory.json']);
  for (const [entries, extensions, isPage] of [[pages, pageExtensions, true], [downloads, downloadExtensions, false]]) {
    for (const entry of entries) {
      object(entry, 'inventory entry', isPage ? ['source', 'route', 'title', 'section'] : ['source', 'route']);
      relativeName(entry.source, 'source');
      relativeName(entry.route, 'route');
      const extension = path.posix.extname(entry.source);
      if (!extensions.has(extension)) throw new Error('Source extension is not permitted');
      if (isPage && !entry.route.split('/').every(segment => /^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(segment))) throw new Error('Invalid page route');
      if (!isPage && path.posix.extname(entry.route) !== extension) throw new Error('Download destination must preserve its permitted extension');
      if (entry.title !== undefined) text(entry.title, 'page title');
      if (entry.section !== undefined) text(entry.section, 'page section');
      const destination = isPage ? `${entry.route}.md` : entry.route;
      // Portable inventories cannot overwrite files on case-insensitive hosts.
      const sourceKey = entry.source.toLowerCase();
      const destinationKey = destination.toLowerCase();
      if (sources.has(sourceKey) || destinations.has(destinationKey)) throw new Error('Duplicate public source or destination');
      sources.add(sourceKey);
      destinations.add(destinationKey);
    }
  }
  // A file destination must never also be an ancestor of another destination.
  for (const destination of destinations) {
    for (const other of destinations) if (other.startsWith(`${destination}/`)) throw new Error('Conflicting public destination paths');
  }
  return { pages, downloads };
}

export function validateQualifications(qualification, pages) {
  object(qualification, 'qualification map', pages.map(page => page.route));
  for (const page of pages) {
    if (!Object.hasOwn(qualification, page.route)) throw new Error(`Missing qualification for ${page.route}`);
    validateQualification(qualification[page.route], page.route);
  }
  return qualification;
}

// Every existing ancestor is checked with lstat. Missing components are allowed
// only for a newly generated destination, never for a source.
async function rejectSymlinks(absolute, allowMissing = false) {
  const parsed = path.parse(absolute);
  let current = parsed.root;
  for (const segment of absolute.slice(parsed.root.length).split(path.sep).filter(Boolean)) {
    current = path.join(current, segment);
    let stat;
    try { stat = await lstat(current); } catch (error) {
      if (allowMissing && error.code === 'ENOENT') return;
      throw error;
    }
    if (stat.isSymbolicLink()) throw new Error(`Symlink is forbidden: ${current}`);
    if (current !== absolute && !stat.isDirectory()) throw new Error('Source/destination ancestor must be a directory');
  }
}

function contained(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative !== '' && !relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative);
}

async function sourceFile(docsRoot, source) {
  const absolute = path.join(docsRoot, source);
  if (!contained(docsRoot, absolute)) throw new Error('Source is outside docsRoot');
  await rejectSymlinks(absolute);
  if (!(await lstat(absolute)).isFile()) throw new Error('Source must be a regular file');
  const canonical = await realpath(absolute);
  if (!contained(docsRoot, canonical)) throw new Error('Canonical source is outside docsRoot');
  return readFile(canonical);
}

/** Stage only allowlisted public content after validating all destructive boundaries. */
export async function prepare({ docsRoot, stagingRoot, pages = publicPages, downloads = publicDownloads, qualification }) {
  if (typeof docsRoot !== 'string' || !path.isAbsolute(docsRoot)) throw new Error('docsRoot must be absolute');
  await rejectSymlinks(docsRoot);
  if (!(await lstat(docsRoot)).isDirectory()) throw new Error('docsRoot must be a directory');
  const canonicalDocs = await realpath(docsRoot);
  const expected = path.join(canonicalDocs, '.vitepress', '.content');
  // Do not normalize arbitrary caller paths into an apparently safe deletion target.
  if (stagingRoot !== expected) throw new Error('Unsafe staging destination; expected canonical docsRoot/.vitepress/.content');
  await rejectSymlinks(expected, true);
  try {
    if (!(await lstat(expected)).isDirectory()) throw new Error('Staging destination must be a directory');
  } catch (error) { if (error.code !== 'ENOENT') throw error; }
  validateInventory(pages, downloads);
  validateQualifications(qualification, pages);

  const outputs = [];
  for (const page of pages) {
    const parsed = matter((await sourceFile(canonicalDocs, page.source)).toString('utf8'), frontmatterOptions);
    const frontmatter = { ...parsed.data, title: page.title ?? parsed.data.title, qualification: qualification[page.route] };
    outputs.push([`${page.route}.md`, matter.stringify(parsed.content, frontmatter)]);
  }
  for (const download of downloads) outputs.push([download.route, await sourceFile(canonicalDocs, download.source)]);
  const inventory = { pages: pages.map(page => ({ ...page, qualification: qualification[page.route] })), downloads };
  outputs.push(['inventory.json', `${JSON.stringify(inventory, null, 2)}\n`]);

  // Recheck ancestors immediately before recursive removal. No unlisted source
  // directory is ever traversed or copied.
  await rejectSymlinks(expected, true);
  await rm(expected, { recursive: true, force: true });
  await mkdir(expected, { recursive: true });
  for (const [destination, content] of outputs) {
    const absolute = path.join(expected, destination);
    await mkdir(path.dirname(absolute), { recursive: true });
    await writeFile(absolute, content);
  }
}

async function main() {
  if (process.versions.node.split('.')[0] !== '24') throw new Error('Public documentation preparation requires Node 24');
  const docsRoot = await realpath(fileURLToPath(new URL('../../', import.meta.url)));
  const qualification = JSON.parse(await readFile(path.join(docsRoot, 'compatibility.json'), 'utf8'));
  await prepare({ docsRoot, stagingRoot: path.join(docsRoot, '.vitepress/.content'), qualification });
  console.log(`Prepared ${publicPages.length} public pages and ${publicDownloads.length} exact downloads.`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main().catch(error => { console.error(error.message); process.exitCode = 1; });
}
