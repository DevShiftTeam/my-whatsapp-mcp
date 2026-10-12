import { readFile, readdir, lstat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parse } from 'parse5';
import { parse as parseJavaScript } from 'acorn';
import MarkdownIt from 'markdown-it';
import parseCSSValues from 'postcss-value-parser';
import { exportPage } from './markdown.mjs';
import { pages, downloads } from '../public-pages.mjs';
import { validateInventory } from './prepare.mjs';
import { resolveSite } from './site.mjs';

function decoded(value) {
  // Inspect common serialized/HTML and repeated URL encodings without executing JS.
  let text = value.replace(/\\u([0-9a-f]{4})/gi, (_, hex) => String.fromCharCode(parseInt(hex, 16)))
    .replace(/\\x([0-9a-f]{2})/gi, (_, hex) => String.fromCharCode(parseInt(hex, 16))).replaceAll('\\/', '/');
  for (let i = 0; i < 5; i++) {
    const next = text.replace(/(?:%[0-9a-f]{2})+/gi, part => { try { return decodeURIComponent(part); } catch { return part; } });
    if (next === text) break;
    text = next;
  }
  return text;
}

function privateContent(content, name) {
  const text = decoded(content);
  if (/PRIVATE[_ -]DOCS[_ -]SENTINEL/i.test(text)) throw new Error(`Private sentinel in ${name}`);
  if (/(?:DEPENDENCY_SECURITY\.md|task-\d+-(?:report|brief)\.md)/i.test(text)) throw new Error(`Internal document in ${name}`);
  // Only this literal, reviewed teaching placeholder is exempted.
  const paths = text.replaceAll('/Users/YOU/', '/EXAMPLE_USER/');
  if (/(?:\/Users\/|\/home\/[A-Za-z0-9_-]+\/|[A-Z]:[\\/]Users[\\/]|file:\/\/|\.superpowers\/|\.codex(?:-b)?\/|rollout_summaries\/)/i.test(paths)) throw new Error(`Private local path in ${name}`);
  if (/(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-[A-Za-z0-9]{32,}|AKIA[0-9A-Z]{16}|-----BEGIN (?:RSA |OPENSSH |EC )?PRIVATE KEY-----)/.test(text)) throw new Error(`Credential pattern in ${name}`);
}

// Share resource extraction between stylesheets, style elements and attributes.
function cssLinks(content) {
  const links = [];
  const cssHref = value => value.replace(/\\([0-9a-f]{1,6})\s?/gi, (_, hex) => String.fromCodePoint(parseInt(hex, 16))).replace(/\\([^\r\n])/g, '$1');
  parseCSSValues(content).walk((node, index, nodes) => {
    if (node.type === 'function' && node.value.toLowerCase() === 'url') {
      links.push(cssHref(node.nodes.filter(part => part.type !== 'space' && part.type !== 'comment').map(part => part.value).join('')));
    }
    if (node.type === 'word' && node.value.toLowerCase() === '@import') {
      const next = nodes.slice(index + 1).find(part => part.type !== 'space' && part.type !== 'comment');
      if (next?.type === 'string') links.push(cssHref(next.value));
    }
    if (node.type === 'function' && ['image-set', '-webkit-image-set'].includes(node.value.toLowerCase())) {
      // Only the first value of each candidate is a resource. type("image/png")
      // and other nested strings are descriptors, not resource references.
      let candidate = true;
      for (const part of node.nodes) {
        if (part.type === 'space' || part.type === 'comment') continue;
        if (part.type === 'div' && part.value === ',') { candidate = true; continue; }
        if (candidate && part.type === 'string') links.push(cssHref(part.value));
        candidate = false;
      }
    }
  });
  return links;
}

function htmlInfo(content) {
  const ids = new Set();
  const links = [];
  function visit(node) {
    if (node.nodeName === '#text') privateContent(node.value, 'HTML text');
    for (const attr of node.attrs ?? []) {
      privateContent(attr.value, 'HTML attribute');
      if (attr.name === 'id' || (node.tagName === 'a' && attr.name === 'name')) ids.add(attr.value);
      if (['href', 'src', 'poster', 'data'].includes(attr.name)) links.push(attr.value);
      if (attr.name === 'style') links.push(...cssLinks(attr.value));
      if (attr.name === 'srcset') links.push(...attr.value.split(',').map(value => value.trim().split(/\s+/)[0]));
    }
    if (node.tagName === 'style') links.push(...cssLinks((node.childNodes ?? []).filter(child => child.nodeName === '#text').map(child => child.value).join('')));
    for (const child of node.childNodes ?? []) visit(child);
    if (node.content) visit(node.content);
  }
  visit(parse(content));
  return { ids, links };
}

async function filesWithin(root, prefix = '') {
  if ((await lstat(root)).isSymbolicLink()) throw new Error('Symlink in artifact');
  const files = [];
  for (const entry of await readdir(root, { withFileTypes: true })) {
    const name = prefix + entry.name;
    if (entry.isSymbolicLink()) throw new Error(`Symlink in artifact: ${name}`);
    if (entry.isDirectory()) files.push(...await filesWithin(path.join(root, entry.name), `${name}/`));
    else if (entry.isFile()) files.push(name);
    else throw new Error(`Nonregular artifact: ${name}`);
  }
  return files;
}

/** Qualify a static artifact against its staged source inventory; never execute emitted JS. */
export async function checkOutput({ distRoot, stagingRoot, expectedInventory, baseURL = resolveSite().baseURL }) {
  const inventory = JSON.parse(await readFile(path.join(stagingRoot, 'inventory.json'), 'utf8'));
  const publicInventory = { pages: inventory.pages.map(({ qualification, ...page }) => page), downloads: inventory.downloads };
  validateInventory(publicInventory.pages, publicInventory.downloads);
  if (expectedInventory && JSON.stringify(publicInventory) !== JSON.stringify(expectedInventory)) throw new Error('Staged inventory differs from explicit sources');
  const files = await filesWithin(distRoot);
  const fileSet = new Set(files);
  const required = new Set(['llms.txt', 'llms-full.txt', 'hashmap.json']);
  for (const page of inventory.pages) { required.add(`${page.route}.html`); required.add(`markdown/${page.route}.md`); }
  for (const item of inventory.downloads) required.add(item.route);
  for (const name of required) if (!fileSet.has(name)) throw new Error(`Missing artifact: ${name}`);
  const hashes = JSON.parse(await readFile(path.join(distRoot, 'hashmap.json'), 'utf8'));
  const hashKeys = inventory.pages.map(page => `${page.route.replaceAll('/', '_')}.md`).sort();
  if (JSON.stringify(Object.keys(hashes).sort()) !== JSON.stringify(hashKeys)) throw new Error('Page hashmap differs from explicit inventory');
  for (const [key, hash] of Object.entries(hashes)) {
    if (!/^[\w-]+$/.test(hash)) throw new Error('Unsafe page hash');
    for (const suffix of ['', '.lean']) required.add(`assets/${key}.${hash}${suffix}.js`);
  }
  for (const name of required) if (!fileSet.has(name)) throw new Error(`Missing artifact: ${name}`);
  // Exact VitePress 1.6 runtime families, not a wildcard assets directory exception.
  const runtime = /^(?:404\.html|vp-icons\.css|assets\/(?:app\.[\w-]+\.js|style\.[\w-]+\.css)|assets\/chunks\/(?:framework|theme|VPLocalSearchBox|@localSearchIndexroot)\.[\w-]+\.js)$/;
  for (const name of files) if (!required.has(name) && !runtime.test(name)) throw new Error(`Unlisted artifact: ${name}`);
  const runtimeFamilies = new Set();
  for (const name of files.filter(name => runtime.test(name))) {
    const family = name.replace(/\.[\w-]+\.(js|css)$/, '.$1');
    if (runtimeFamilies.has(family)) throw new Error(`Duplicate runtime asset: ${family}`);
    runtimeFamilies.add(family);
  }
  const texts = new Map();
  const html = new Map();
  for (const name of files) {
    const bytes = await readFile(path.join(distRoot, name));
    const content = bytes.toString('utf8');
    privateContent(content, name);
    texts.set(name, content);
    if (name.endsWith('.html')) html.set(name, htmlInfo(content));
  }
  for (const item of inventory.downloads) {
    const source = await readFile(path.join(stagingRoot, item.route));
    if (!source.equals(await readFile(path.join(distRoot, item.route)))) throw new Error(`Copied asset differs from explicit source: ${item.route}`);
  }
  const base = new URL(baseURL);
  function checkLink(href, from) {
    if (!href || href.startsWith('data:')) return;
    privateContent(href, from);
    const url = new URL(href, new URL(from, base));
    if (!['https:', 'http:', 'mailto:'].includes(url.protocol)) throw new Error(`Unsafe link scheme in ${from}`);
    if (url.origin !== base.origin) return;
    if (!url.pathname.startsWith(base.pathname)) throw new Error(`Local link outside site base: ${href} in ${from}`);
    const target = decodeURIComponent(url.pathname.slice(base.pathname.length)) || 'index.html';
    if (!fileSet.has(target)) throw new Error(`Missing local target ${target} linked from ${from}`);
    if (url.hash && html.has(target) && !html.get(target).ids.has(decodeURIComponent(url.hash.slice(1)))) throw new Error(`Missing anchor ${url.hash} in ${target} linked from ${from}`);
  }
  for (const [name, info] of html) for (const href of info.links) checkLink(href, name);
  for (const name of files.filter(name => name.endsWith('.css'))) {
    for (const href of cssLinks(texts.get(name))) checkLink(href, name);
  }
  const indexedRoutes = new Set();
  let searchChunks = 0;
  for (const name of files.filter(name => name.endsWith('.js'))) {
    const ast = parseJavaScript(texts.get(name), { ecmaVersion: 'latest', sourceType: 'module' });
    function visit(node) {
      if (!node || typeof node !== 'object') return;
      if (['ImportDeclaration', 'ExportNamedDeclaration', 'ExportAllDeclaration', 'ImportExpression'].includes(node.type) && node.source?.type === 'Literal') checkLink(node.source.value, name);
      for (const value of Object.values(node)) {
        if (Array.isArray(value)) for (const child of value) visit(child);
        else if (value && typeof value === 'object') visit(value);
      }
    }
    visit(ast);
    if (name.includes('/@localSearchIndexroot.')) {
      searchChunks++;
      const declaration = ast.body.find(node => node.type === 'VariableDeclaration');
      const literal = declaration?.declarations[0]?.init;
      if (literal?.type !== 'Literal' || typeof literal.value !== 'string') throw new Error('Unexpected generated search format');
      const index = JSON.parse(literal.value);
      if (!index.documentIds || Object.keys(index.documentIds).length !== index.documentCount) throw new Error('Invalid search document inventory');
      for (const href of Object.values(index.documentIds)) {
        checkLink(href, name);
        const route = new URL(href, base).pathname.slice(base.pathname.length).replace(/\.html$/, '');
        if (!inventory.pages.some(page => page.route === route)) throw new Error(`Unlisted search page: ${route}`);
        indexedRoutes.add(route);
      }
    }
  }
  if (searchChunks !== 1 || indexedRoutes.size !== inventory.pages.length) throw new Error('Search inventory does not cover all public pages');
  const parser = new MarkdownIt({ html: true });
  parser.validateLink = () => true;
  function tokensLinks(tokens, from) {
    for (const token of tokens) {
      for (const attribute of ['href', 'src']) if (token.attrGet(attribute)) checkLink(token.attrGet(attribute), from);
      if (token.type === 'html_block' || token.type === 'html_inline') for (const href of htmlInfo(token.content).links) checkLink(href, from);
      if (token.children) tokensLinks(token.children, from);
    }
  }
  for (const name of files.filter(name => name.endsWith('.md') || name === 'llms.txt')) {
    // Exported fragment-only links refer to the corresponding HTML document.
    const from = name.startsWith('markdown/') ? name.slice(9).replace(/\.md$/, '.html') : 'index.html';
    tokensLinks(parser.parse(texts.get(name), {}), from);
  }
  const combined = inventory.pages.map(page => texts.get(`markdown/${page.route}.md`)).join('\n');
  if (texts.get('llms-full.txt') !== combined) throw new Error('Combined bundle differs from standalone exports');
  if (expectedInventory) for (const page of inventory.pages) {
    const expected = exportPage({ ...page, markdown: await readFile(path.join(stagingRoot, `${page.route}.md`), 'utf8'), baseURL, inventory: publicInventory });
    if (texts.get(`markdown/${page.route}.md`) !== expected) throw new Error(`Markdown differs from staged source: ${page.route}`);
  }
  return { pages: inventory.pages.length, downloads: inventory.downloads.length, files: files.length };
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const root = fileURLToPath(new URL('../', import.meta.url));
  checkOutput({ distRoot: path.join(root, 'dist'), stagingRoot: path.join(root, '.content'), expectedInventory: { pages, downloads } })
    .then(result => console.log(`Qualified ${result.pages} pages, ${result.downloads} downloads, ${result.files} static files.`))
    .catch(error => { console.error(error.message); process.exitCode = 1; });
}
