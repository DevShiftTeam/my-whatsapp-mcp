import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

// Narrow controller disposition in DEPENDENCY_SECURITY.md, not blanket audit
// suppression. New packages, advisory paths or severity changes fail closed.
const accepted = {
  argparse: ['moderate', 'sprintf-js'],
  esbuild: ['moderate', 'https://github.com/advisories/GHSA-67mh-4wv8-2f99'],
  'gray-matter': ['moderate', 'js-yaml'],
  'js-yaml': ['moderate', 'argparse'],
  'sprintf-js': ['moderate', 'https://github.com/advisories/GHSA-hp3w-g68c-fv3c'],
  vite: ['high', 'esbuild', 'https://github.com/advisories/GHSA-4w7w-66w2-5vf9', 'https://github.com/advisories/GHSA-v6wh-96g9-6wx3', 'https://github.com/advisories/GHSA-fx2h-pf6j-xcff', 'https://github.com/advisories/GHSA-698v-4r3x-3vcv', 'https://github.com/advisories/GHSA-r825-w8mm-5pxw', 'https://github.com/advisories/GHSA-587h-7wmj-h5pm', 'https://github.com/advisories/GHSA-9jrq-w75r-8gcw', 'https://github.com/advisories/GHSA-rq7h-c2jc-7f22'],
  vitepress: ['moderate', 'vite']
};

export function qualifyAudit(report) {
  if (report.error || report.auditReportVersion !== 2 || !report.vulnerabilities || Array.isArray(report.vulnerabilities) || typeof report.vulnerabilities !== 'object') throw new Error('Invalid audit response');
  for (const [name, finding] of Object.entries(report.vulnerabilities)) {
    if (!Object.hasOwn(accepted, name)) throw new Error(`New dependency finding requires review: ${name}`);
    const [severity, ...via] = accepted[name];
    if (finding.severity !== severity) throw new Error(`Changed severity requires review: ${name}`);
    if (!Array.isArray(finding.via) || !finding.via.length) throw new Error('Invalid audit advisory paths');
    for (const item of finding.via) if (!via.includes(typeof item === 'string' ? item : item.url)) throw new Error(`New advisory requires review: ${name}`);
  }
  return Object.keys(report.vulnerabilities).length;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  try {
    const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
    const audit = spawnSync(npm, ['audit', '--json'], { encoding: 'utf8' });
    if (audit.error || ![0, 1].includes(audit.status)) throw new Error('npm audit execution failed');
    const findings = qualifyAudit(JSON.parse(audit.stdout));
    const stable = spawnSync(npm, ['view', 'vitepress', 'dist-tags.latest', '--json'], { encoding: 'utf8' });
    if (stable.error || stable.status !== 0 || JSON.parse(stable.stdout) !== '1.6.4') throw new Error('Stable VitePress baseline changed or could not be checked; fresh security review required');
    console.log(`Dependency disposition matched: ${findings} known findings remain; this is not a clean audit.`);
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
