import { lstat, readFile, mkdir, writeFile, rm } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { validateInventory, validateQualification } from './prepare.mjs';
import { exportPage, canonicalURL, markdownURL, validateBase, DEFAULT_BASE_URL } from './markdown.mjs';

async function safePath(absolute, allowMissing = false) {
  const root = path.parse(absolute).root;
  let current = root;
  for (const segment of absolute.slice(root.length).split(path.sep).filter(Boolean)) {
    current = path.join(current, segment);
    let stat;
    try { stat = await lstat(current); } catch (error) {
      if (allowMissing && error.code === 'ENOENT') return;
      throw error;
    }
    if (stat.isSymbolicLink()) throw new Error('Symlink is forbidden in Markdown exports');
    if (current !== absolute && !stat.isDirectory()) throw new Error('Export ancestor must be a directory');
  }
}

/** Read only staged inventory entries; leave the renderer's other dist files intact. */
export async function exportDocumentation({ stagingRoot, distRoot, baseURL = DEFAULT_BASE_URL }) {
  validateBase(baseURL, true);
  if (!path.isAbsolute(stagingRoot || '') || path.basename(stagingRoot) !== '.content' || path.basename(path.dirname(stagingRoot)) !== '.vitepress') throw new Error('Unsafe export staging root');
  if (distRoot !== path.join(path.dirname(stagingRoot), 'dist')) throw new Error('Unsafe export destination; expected staging sibling dist');
  await safePath(stagingRoot);
  const inventoryPath = path.join(stagingRoot, 'inventory.json');
  await safePath(inventoryPath);
  const inventory = JSON.parse(await readFile(inventoryPath, 'utf8'));
  if (!inventory || Object.keys(inventory).some(key => !['pages', 'downloads'].includes(key)) || !Array.isArray(inventory.pages)) throw new Error('Invalid staged inventory');
  validateInventory(inventory.pages.map(({ qualification, ...page }) => page), inventory.downloads);
  const linkInventory = { pages: inventory.pages.map(({ qualification, ...page }) => page), downloads: inventory.downloads };
  const outputs = [];
  for (const page of inventory.pages) {
    validateQualification(page.qualification);
    const sourcePath = path.join(stagingRoot, `${page.route}.md`);
    await safePath(sourcePath);
    if (!(await lstat(sourcePath)).isFile()) throw new Error('Staged page must be a regular file');
    outputs.push({ ...page, content: exportPage({ ...page, markdown: await readFile(sourcePath, 'utf8'), baseURL, inventory: linkInventory }) });
  }
  const assets = [];
  for (const download of inventory.downloads) {
    const sourcePath = path.join(stagingRoot, download.route);
    await safePath(sourcePath);
    if (!(await lstat(sourcePath)).isFile()) throw new Error('Staged download must be a regular file');
    const destination = path.join(distRoot, download.route);
    await safePath(destination, true);
    assets.push({ destination, bytes: await readFile(sourcePath) });
  }
  const markdownRoot = path.join(distRoot, 'markdown');
  const indexPath = path.join(distRoot, 'llms.txt');
  const fullPath = path.join(distRoot, 'llms-full.txt');
  for (const output of [distRoot, markdownRoot, indexPath, fullPath]) await safePath(output, true);
  // All reads, transforms and boundary checks finish before any generated output changes.
  await mkdir(distRoot, { recursive: true });
  await rm(markdownRoot, { recursive: true, force: true });
  await mkdir(markdownRoot);
  for (const page of outputs) {
    const destination = path.join(markdownRoot, `${page.route}.md`);
    await mkdir(path.dirname(destination), { recursive: true });
    await writeFile(destination, page.content);
  }
  for (const asset of assets) {
    await mkdir(path.dirname(asset.destination), { recursive: true });
    await writeFile(asset.destination, asset.bytes);
  }
  const index = '# My WhatsApp documentation\n\n' + outputs.map(page => `- [${page.title}](${canonicalURL(page.route, baseURL)}) — [Markdown](${markdownURL(page.route, baseURL)}) — ${page.qualification.availability}; ${page.qualification.status}; reviewed ${page.qualification.reviewedAt}`).join('\n') + '\n';
  await writeFile(indexPath, index);
  await writeFile(fullPath, outputs.map(page => page.content).join('\n'));
  return { pages: outputs.length, markdownRoot, indexPath, fullPath };
}

async function main() {
  if (process.versions.node.split('.')[0] !== '24') throw new Error('Public documentation export requires Node 24');
  if (process.argv.length > 3) throw new Error('Usage: node scripts/export.mjs [https://canonical-base/]');
  const vitepressRoot = fileURLToPath(new URL('../', import.meta.url));
  const result = await exportDocumentation({ stagingRoot: path.join(vitepressRoot, '.content'), distRoot: path.join(vitepressRoot, 'dist'), baseURL: process.argv[2] ?? DEFAULT_BASE_URL });
  console.log(`Exported ${result.pages} public Markdown pages and deterministic AI bundles.`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) main().catch(error => { console.error(error.message); process.exitCode = 1; });
