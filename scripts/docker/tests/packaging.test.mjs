import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
const read = p => readFileSync(new URL('../../../'+p, import.meta.url),'utf8');
test('runtime is non-root, immutable and directly executable',()=>{
 const file=read('Dockerfile');
 assert.match(file,/USER 10001:10001/); assert.match(file,/ENTRYPOINT \["\/usr\/local\/bin\/wa-indexer"\]/);
 assert.match(file,/CGO_ENABLED=1/); assert.match(file,/sqlite_fts5/); assert.match(file,/type=cache/);
 const runtime=file.split('AS runtime')[1]; assert.ok(runtime); assert.doesNotMatch(runtime,/^RUN /m);
});
test('Compose isolates localhost and persistent writes',()=>{
 const file=read('compose.yaml');
 for(const v of ['127.0.0.1:', 'read_only: true', 'no-new-privileges:true', 'cap_drop: [ALL]', 'size=16m', 'start_interval: 1s']) assert.ok(file.includes(v));
 assert.doesNotMatch(file,/latest|privileged:|docker.sock|network_mode/);
});
test('base pins include both target architectures and no release claim',()=>{
 const pins=JSON.parse(read('packaging/docker-images.json'));
 for(const p of Object.values(pins.bases)){assert.match(p.reference,/@sha256:[a-f0-9]{64}$/);assert.deepEqual(p.platforms,['linux/amd64','linux/arm64']);}
 assert.equal(pins.status,'source-build-preview');
});

test('validation workflows have no registry or release publication side effect',()=>{
 const file=read('.github/workflows/docker.yml')+read('.github/workflows/docker-native.yml');
 assert.doesNotMatch(file,/packages:\s*write|contents:\s*write|tags:\s*\[|docker\/login-action|push:\s*true|scripts\/docker\/publish\.mjs/);
 assert.match(file,/runner: ubuntu-24\.04-arm/);assert.match(file,/runner: ubuntu-24\.04/);
 assert.ok(read('.github/workflows/ci.yml').includes('make test'));
});
test('optional transcription retains private shared-loopback configuration',()=>{
 const file=read('compose.transcription.yaml');
 assert.match(file,/network_mode: service:wa-indexer/);assert.match(file,/profiles: \[transcription\]/);
 assert.match(file,/--transcription-env-file/);assert.doesNotMatch(file,/depends_on:|ports:|TOKEN=/);
 assert.doesNotMatch(read('compose.yaml'),/transcription|depends_on/);
});
test('build helpers remain opt-in runtime providers with all privacy tags',()=>{
 assert.match(read('scripts/app/build.sh'),/fts5,sqlite_fts5,ts_omit_logtail/);
 assert.match(read('Makefile'),/TAGS := fts5,sqlite_fts5,ts_omit_logtail/);
 assert.match(read('Dockerfile'),/sqlite_fts5,ts_omit_logtail/);
 assert.doesNotMatch(read('scripts/app/build.sh'),/rtk/);
});

test('retained synthetic document fixture uses exact metadata lookup with guarded original checks',()=>{
 const file=read('scripts/docker/tests/docker.spec.ts');
 assert.match(file,/search_attachments',args:\{filename:'synthetic-acceptance\.pdf'\}/);
 assert.doesNotMatch(file,/query:'SYNTHETIC_DOCUMENT_ACCEPTANCE'|ownerSearch\('SYNTHETIC_DOCUMENT_ACCEPTANCE'\)/);
 for(const code of ['retained_document_out_of_scope','retained_document_metadata','retained_original_download','expired_original_denied','revoked_original_denied'])assert.ok(file.includes(code),code);
 assert.ok(file.includes('download.bytes===83'));
});

test('Cloudflare source and guides share narrow client routes without owner access',()=>{
 const example=read('docs/clients/examples/cloudflared.yml');
 const pattern=example.match(/path: '([^']+)'/)[1];const allowed=new RegExp(pattern);
 const ticket=Buffer.alloc(32,1).toString('base64url');
 for(const path of ['/mcp','/mcp/attachments/'+ticket,'/api/send/message','/api/send/media','/api/send/voice','/api/send/status'])assert.equal(allowed.test(path),true,path);
 for(const path of ['/','/api/setup','/api/policy','/api/connections','/api/send/other','/api/send/message/','/mcp/attachments/short','/mcp/attachments/'+ticket+'x','/mcp/attachments/'+'B'.repeat(43),'/mcp/attachments/'+ticket+'/extra','/bootstrap','/healthz','/api/remote/provider','/mcp/stdio'])assert.equal(allowed.test(path),false,path);
 for(const name of ['docs/clients/remote-access.md','docs/remote/cloudflare.md'])assert.ok(read(name).includes(pattern),name);
 assert.ok(example.includes('service: http_status:404'));
});
test('setup and maintenance records retain preview status without claiming runtime success',()=>{
 const records=JSON.parse(read('docs/compatibility.json'));
 for(const route of ['start-here','install/docker','maintenance/backups','maintenance/updates']){
  assert.equal(records[route].availability,'preview');
  assert.equal(records[route].status,'documentation_reviewed');
  assert.deepEqual(records[route].tests,[]);
 }
});
test('both native jobs require normal serve while demo benchmark selects visible Archive',()=>{
 assert.match(read('.github/workflows/docker-native.yml'),/benchmark\.mjs[^\n]+--mode serve/);
 const file=read('scripts/docker/benchmark.mjs');
 assert.match(file,/getByRole\('button',\{name:'Archive',exact:true\}\)\.click\(\);await page\.locator\('#search-query'\)/);
 assert.match(file,/normalServe:mode==='serve'/);
});
test('filesystem helper has one reachable downloader plus all foreign-volume fixtures',()=>{
 const file=read('scripts/docker/helper/fs/main.go');
 assert.equal((file.match(/case "attachment-download":/g)||[]).length,1);
 for(const name of ['bad-owner','bad-nonempty','bad-symlink'])assert.ok(file.includes('case "'+name+'":'));
});
