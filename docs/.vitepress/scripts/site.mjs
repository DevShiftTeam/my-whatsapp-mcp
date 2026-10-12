import { validateBase, canonicalURL } from './urls.mjs';

/** A single checked location contract for render, exports and static preview. */
export function resolveSite(env = process.env) {
  const repository = env.GITHUB_REPOSITORY ?? 'DevShiftTeam/my-whatsapp-mcp';
  if (!/^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?\/my-whatsapp-mcp$/.test(repository)) throw new Error('Invalid documentation repository; expected owner/my-whatsapp-mcp');
  const [owner, name] = repository.split('/');
  const base = validateBase(env.DOCS_BASE ?? `/${name}/`);
  if (base !== `/${name}/`) throw new Error('Documentation base must match the repository');
  const expectedURL = `https://${owner.toLowerCase()}.github.io${base}`;
  const baseURL = validateBase(env.DOCS_BASE_URL ?? expectedURL, true);
  if (baseURL !== expectedURL) throw new Error('Documentation canonical base must match the repository');
  canonicalURL('index', baseURL);
  return { repository, base, baseURL, repositoryURL: `https://github.com/${repository}` };
}
