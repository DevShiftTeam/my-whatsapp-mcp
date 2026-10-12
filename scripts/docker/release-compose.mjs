import {readFileSync,writeFileSync,mkdirSync} from 'node:fs';
import {pathToFileURL} from 'node:url';
import {releaseVersion} from './release-policy.mjs';
import {digest} from './oci.mjs';
import {check} from './lib.mjs';

export function releaseCompose(source,version,reference) {
  releaseVersion('refs/tags/'+version);
  check(/^ghcr\.io\/devshiftteam\/my-whatsapp-mcp@sha256:[a-f0-9]{64}$/.test(reference),'release_compose_digest_required');
  check((source.match(/^    image: wa-indexer:development$/gm)||[]).length===1,'release_compose_template_changed');
  return '# Qualified keyword-search image; AI capabilities are not bundled.\n# Requires Engine 28+ and Compose 2.20.2+.\n'+source.replace(/^#.*\n/gm,'').replace('    image: wa-indexer:development','    image: '+reference.replace('@',':'+version+'@'));
}
if(import.meta.url===pathToFileURL(process.argv[1]||'').href) {
  check(process.env.NATIVE_VERIFICATION==='PASS','release_compose_native_gate');
  const compose=releaseCompose(readFileSync('compose.yaml','utf8'),process.env.RELEASE_VERSION,process.env.RELEASE_REFERENCE);
  mkdirSync('.artifacts/release-compose',{recursive:true});
  writeFileSync('.artifacts/release-compose/compose.yaml',compose);
  writeFileSync('.artifacts/release-compose/compose.yaml.sha256',digest(compose).slice(7)+'  compose.yaml\n');
}
