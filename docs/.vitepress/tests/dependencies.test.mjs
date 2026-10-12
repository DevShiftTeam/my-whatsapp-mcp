import { test } from 'node:test';
import assert from 'node:assert/strict';
import { qualifyAudit } from '../scripts/check-dependencies.mjs';

const accepted = () => ({ auditReportVersion: 2, vulnerabilities: { esbuild: { severity: 'moderate', via: [{ url: 'https://github.com/advisories/GHSA-67mh-4wv8-2f99' }] } } });
test('known static-only advisory is reported without claiming a clean audit', () => {
  assert.equal(qualifyAudit(accepted()), 1);
});
test('new dependency, advisory and severity require fresh review', () => {
  const added = accepted(); added.vulnerabilities.new_package = { severity: 'low', via: [] };
  assert.throws(() => qualifyAudit(added), /New dependency/);
  const advisory = accepted(); advisory.vulnerabilities.esbuild.via.push({ url: 'https://github.com/advisories/GHSA-new' });
  assert.throws(() => qualifyAudit(advisory), /New advisory/);
  const severity = accepted(); severity.vulnerabilities.esbuild.severity = 'critical';
  assert.throws(() => qualifyAudit(severity), /Changed severity/);
});
test('audit failures and malformed reports cannot qualify', () => {
  for (const result of [{ error: { code: 'ENOTFOUND' } }, {}, { auditReportVersion: 2, vulnerabilities: null }]) assert.throws(() => qualifyAudit(result), /Invalid audit/);
});
