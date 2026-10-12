import { test } from 'node:test';
import assert from 'node:assert/strict';
import { guideNotice } from '../scripts/guide-notice.mjs';
import { exportPage } from '../scripts/markdown.mjs';

test('the local source setup retains its untested warning in Markdown', () => {
  const qualification = { availability: 'preview', status: 'documentation_reviewed', reviewedAt: '2026-10-12', tests: [] };
  const notice = guideNotice('setup', qualification);
  assert.equal(notice.title, 'Preview setup');
  const exported = exportPage({ source: 'setup.md', route: 'setup', title: 'Local setup', markdown: '# Local setup\n\nBuild the app.\n', qualification });
  assert.ok(exported.includes(notice.text));
  assert.doesNotMatch(exported, /documentation_reviewed|reviewedAt|"tests"/);
  assert.equal(guideNotice('everyday/search', qualification), null);
});
