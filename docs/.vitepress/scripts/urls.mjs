export const DEFAULT_BASE_URL = 'https://devshiftteam.github.io/my-whatsapp-mcp/';
const routePattern = /^[A-Za-z0-9][A-Za-z0-9_-]*(?:\/[A-Za-z0-9][A-Za-z0-9_-]*)*$/;

/** Shared URL construction for exports and the public theme. */
export function validateBase(base, absolute = false) {
  if (typeof base !== 'string' || !base.endsWith('/') || /[\s\\\x00-\x1f]/.test(base)) throw new Error('Unsafe documentation base');
  if (!absolute && /^\/(?!\/)/.test(base)) {
    if (base.split('/').some(part => part === '.' || part === '..') || /[?#%]/.test(base)) throw new Error('Unsafe documentation base');
    return base;
  }
  let url;
  try { url = new URL(base); } catch { throw new Error('Unsafe documentation base URL'); }
  if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash || url.href !== base || /%/.test(url.pathname)) throw new Error('Unsafe documentation base URL');
  return base;
}

function routePath(route) {
  if (typeof route !== 'string' || !routePattern.test(route)) throw new Error('Invalid public route');
  return route;
}

export function canonicalURL(route, baseURL = DEFAULT_BASE_URL) {
  return `${validateBase(baseURL, true)}${routePath(route)}.html`;
}

export function markdownURL(route, base) {
  return `${validateBase(base)}markdown/${routePath(route)}.md`;
}
