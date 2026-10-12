import { test } from 'node:test';
import assert from 'node:assert/strict';
import { explicitIndexLink, searchField } from '../scripts/explicit-routes.mjs';

test('index Markdown links retain explicit HTML without changing external links or fragments', () => {
  assert.equal(explicitIndexLink('../remote/index.md#choice'), '../remote/index.html#choice');
  assert.equal(explicitIndexLink('index.md'), 'index.html');
  assert.equal(explicitIndexLink('https://example.com/index.md'), 'https://example.com/index.md');
  assert.equal(explicitIndexLink('#index.md'), '#index.md');
});

test('search IDs use explicit root and nested index HTML and retain other fields verbatim', () => {
  assert.equal(searchField({ id: '/my-whatsapp-mcp/remote/#choice' }, 'id'), '/my-whatsapp-mcp/remote/index.html#choice');
  assert.equal(searchField({ id: '/my-whatsapp-mcp/#home' }, 'id'), '/my-whatsapp-mcp/index.html#home');
  assert.equal(searchField({ id: '/my-whatsapp-mcp/install/docker.html#docker' }, 'id'), '/my-whatsapp-mcp/install/docker.html#docker');
  assert.equal(searchField({ text: 'a/b/' }, 'text'), 'a/b/');
});
