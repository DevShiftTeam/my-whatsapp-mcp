import assert from 'node:assert/strict';
import { test } from 'node:test';
import { resolveSite } from '../scripts/site.mjs';
import * as shared from '../scripts/urls.mjs';
import * as legacy from '../scripts/markdown.mjs';

test('one repository contract controls HTML and canonical Markdown URLs', () => {
  assert.deepEqual(resolveSite({}), { repository: 'DevShiftTeam/my-whatsapp-mcp', base: '/my-whatsapp-mcp/', baseURL: 'https://devshiftteam.github.io/my-whatsapp-mcp/', repositoryURL: 'https://github.com/DevShiftTeam/my-whatsapp-mcp' });
  assert.equal(resolveSite({ GITHUB_REPOSITORY: 'Example-Team/my-whatsapp-mcp' }).baseURL, 'https://example-team.github.io/my-whatsapp-mcp/');
  assert.equal(resolveSite({ DOCS_BASE: '/my-whatsapp-mcp/' }).base, '/my-whatsapp-mcp/');
});

test('malformed or conflicting repository and base overrides fail closed', () => {
  for (const repository of ['', 'org/other', '../my-whatsapp-mcp', 'org/my-whatsapp-mcp/extra', 'org%2f/my-whatsapp-mcp', '-org/my-whatsapp-mcp', 'org./my-whatsapp-mcp']) {
    assert.throws(() => resolveSite({ GITHUB_REPOSITORY: repository }), /repository/i);
  }
  for (const base of ['', '/', '//evil/', '/other/', '/my-whatsapp-mcp/../', '/my-whatsapp-mcp/%2e/']) assert.throws(() => resolveSite({ DOCS_BASE: base }));
  for (const baseURL of ['https://evil.test/my-whatsapp-mcp/', 'http://devshiftteam.github.io/my-whatsapp-mcp/', 'https://devshiftteam.github.io/my-whatsapp-mcp/?x=1']) assert.throws(() => resolveSite({ DOCS_BASE_URL: baseURL }));
});

test('Task 2 exports retain the identical browser-safe URL implementations', () => {
  for (const name of ['DEFAULT_BASE_URL', 'canonicalURL', 'markdownURL', 'validateBase']) assert.equal(legacy[name], shared[name]);
  assert.equal(shared.markdownURL('install/docker-compose', '/'), '/markdown/install/docker-compose.md');
  assert.equal(legacy.markdownURL('install/docker-compose', '/my-whatsapp-mcp/'), '/my-whatsapp-mcp/markdown/install/docker-compose.md');
});
