import { createServer } from 'node:http';
import { lstat, readFile, realpath } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { pages, downloads } from '../public-pages.mjs';
import { resolveSite } from './site.mjs';

const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.json': 'application/json; charset=utf-8', '.md': 'text/plain; charset=utf-8', '.txt': 'text/plain; charset=utf-8', '.svg': 'image/svg+xml', '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.webp': 'image/webp', '.ico': 'image/x-icon', '.woff2': 'font/woff2', '.woff': 'font/woff', '.pdf': 'application/pdf', '.yaml': 'text/plain; charset=utf-8', '.yml': 'text/plain; charset=utf-8', '.zip': 'application/zip' };
const outputPaths = new Set(['404.html', 'hashmap.json', 'vp-icons.css', 'llms.txt', 'llms-full.txt', ...pages.flatMap(page => [`${page.route}.html`, `markdown/${page.route}.md`]), ...downloads.map(download => download.route)]);

async function noSymlinks(absolute) {
  let current = path.parse(absolute).root;
  for (const segment of absolute.slice(current.length).split(path.sep).filter(Boolean)) {
    current = path.join(current, segment);
    const stat = await lstat(current);
    if (stat.isSymbolicLink()) throw new Error('Symlink is forbidden in static preview');
    if (current !== absolute && !stat.isDirectory()) throw new Error('Invalid static ancestor');
  }
}

/** Static-only server: no Vite imports, middleware, directory listing or SPA fallback. */
export async function createPreview({ distRoot, port = 4173, env = process.env }) {
  const { base } = resolveSite(env);
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error('Invalid preview port');
  if (!path.isAbsolute(distRoot || '') || path.basename(distRoot) !== 'dist') throw new Error('Preview requires an absolute dist root');
  await noSymlinks(distRoot);
  const root = await realpath(distRoot);
  if (!(await lstat(root)).isDirectory()) throw new Error('Static dist is not a directory');
  const server = createServer(async (request, response) => {
    response.setHeader('X-Content-Type-Options', 'nosniff');
    response.setHeader('Cache-Control', 'no-store');
    // Do not expose the local preview to cross-origin browser scripts.
    response.setHeader('Cross-Origin-Resource-Policy', 'same-origin');
    if (!['GET', 'HEAD'].includes(request.method)) {
      response.writeHead(405, { Allow: 'GET, HEAD' }); response.end('Method not allowed'); return;
    }
    const target = (request.url ?? '').split('?')[0];
    const fail = () => { response.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' }); response.end(request.method === 'HEAD' ? '' : 'Not found'); };
    if (!target.startsWith(base) || /[%\\#\x00-\x20]/.test(target)) { fail(); return; }
    let relative = target.slice(base.length);
    if (relative === '') relative = 'index.html';
    const searchChunk = /^assets\/chunks\/@localSearchIndexroot\.[\w-]+\.js$/.test(relative);
    if (!searchChunk && relative.split('/').some(part => !/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(part))) { fail(); return; }
    const extension = path.extname(relative);
    if (!outputPaths.has(relative) && !(relative.startsWith('assets/') && ['.js', '.css', '.json', '.svg', '.png', '.jpg', '.jpeg', '.webp', '.ico', '.woff', '.woff2'].includes(extension))) { fail(); return; }
    const absolute = path.join(root, relative);
    if (!absolute.startsWith(`${root}${path.sep}`)) { fail(); return; }
    try {
      await noSymlinks(absolute);
      if (!(await lstat(absolute)).isFile()) { fail(); return; }
      const resolved = await realpath(absolute);
      if (!resolved.startsWith(`${root}${path.sep}`)) { fail(); return; }
      const body = await readFile(resolved);
      response.writeHead(200, { 'Content-Type': types[extension] ?? 'application/octet-stream', 'Content-Length': body.length });
      response.end(request.method === 'HEAD' ? undefined : body);
    } catch { fail(); }
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => { server.removeListener('error', reject); resolve(); });
  });
  return server;
}

async function main() {
  const args = process.argv.slice(2);
  if (args.length && (args.length !== 2 || args[0] !== '--port' || !/^\d{1,5}$/.test(args[1]))) throw new Error('Usage: npm run preview -- [--port 4173]; host overrides are forbidden');
  const site = resolveSite();
  const server = await createPreview({ distRoot: fileURLToPath(new URL('../dist', import.meta.url)), port: args.length ? Number(args[1]) : 4173 });
  console.log(`Static documentation: http://127.0.0.1:${server.address().port}${site.base}`);
  for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => { server.close(); });
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) main().catch(error => { console.error(error.message); process.exitCode = 1; });
