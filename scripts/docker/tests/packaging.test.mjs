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
