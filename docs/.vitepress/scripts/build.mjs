import { spawnSync } from 'node:child_process';
import { readFile, realpath } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { prepare } from './prepare.mjs';
import { exportDocumentation } from './export.mjs';
import { resolveSite } from './site.mjs';
import { checkOutput } from './check-output.mjs';
import { pages, downloads } from '../public-pages.mjs';

if (process.versions.node.split('.')[0] !== '24') throw new Error('Documentation build requires Node 24');
const site = resolveSite();
const root = fileURLToPath(new URL('../', import.meta.url));
const docsRoot = await realpath(path.join(root, '..'));
const stagingRoot = path.join(root, '.content');
const qualification = JSON.parse(await readFile(path.join(docsRoot, 'compatibility.json'), 'utf8'));
await prepare({ docsRoot, stagingRoot, qualification });
console.log('Prepared allowlisted documentation.');
// Only the static build CLI is authorized. Never start Vite server middleware.
const result = spawnSync(process.execPath, [path.join(root, 'node_modules/vitepress/bin/vitepress.js'), 'build', '..'], {
  cwd: root, stdio: 'inherit', env: { ...process.env, GITHUB_REPOSITORY: site.repository, DOCS_BASE: site.base, DOCS_BASE_URL: site.baseURL }
});
if (result.error) throw result.error;
if (result.status !== 0) throw new Error(`VitePress build failed (${result.status ?? result.signal})`);
const output = await exportDocumentation({ stagingRoot, distRoot: path.join(root, 'dist'), baseURL: site.baseURL });
console.log(`Exported ${output.pages} Markdown pages and documentation bundles.`);
const checked = await checkOutput({ stagingRoot, distRoot: path.join(root, 'dist'), baseURL: site.baseURL, expectedInventory: { pages, downloads } });
console.log(`Qualified ${checked.pages} pages, ${checked.downloads} downloads, ${checked.files} static files.`);
