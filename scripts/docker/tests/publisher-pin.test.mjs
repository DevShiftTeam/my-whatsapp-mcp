import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {pins,classifyPullFailure,verifyPublisher} from '../oci-tools.mjs';

// Primary-registry index retrieved 2026-10-13 using an anonymous Registry v2
// pull token, then fetched by its digest. The retired 9ba3be83 digest returned
// authenticated 404 MANIFEST_UNKNOWN on that date. These tests check the
// committed fixture only and do not assert current registry availability.
const raw=readFileSync(new URL('./fixtures/skopeo-v1.22.3-index.json',import.meta.url));
const manifest=JSON.parse(raw);
const digest='sha256:'+createHash('sha256').update(raw).digest('hex');

test('Skopeo tool pin matches the reviewed versioned native-platform index fixture',()=>{
 assert.equal(manifest.schemaVersion,2);
 assert.equal(manifest.mediaType,'application/vnd.oci.image.index.v1+json');
 assert.deepEqual(manifest.manifests.map(entry=>entry.platform.os+'/'+entry.platform.architecture).sort(),['linux/amd64','linux/arm64','linux/ppc64le','linux/s390x']);
 for(const entry of manifest.manifests){assert.match(entry.digest,/^sha256:[a-f0-9]{64}$/);assert.ok(entry.size>0);}
 assert.equal(pins.ci.publisher,'quay.io/skopeo/stable:v1.22.3@'+digest);
 assert.equal(pins.ci.publisherVersion,'skopeo version 1.22.3 commit: 92397d5ba399784c6e2f9f229a4244b724743dba');
});

const fakeCredential='FAKE_CREDENTIAL_ONLY';
const fakePath='/Users/fake-operator/.docker/config.json';
const fakeURL='https://fake-user:'+fakeCredential+'@registry.invalid/v2/fake/manifests?token=FAKE_TOKEN';

test('Publisher pull failures map to fixed diagnostic codes',()=>{
  for(const [stderr,code] of [
    ['Error response from daemon: manifest unknown','publisher_manifest_unavailable'],
    ['Error response from daemon: MANIFEST_UNKNOWN','publisher_manifest_unavailable'],
    ['received unexpected HTTP status: 404 Not Found','publisher_manifest_unavailable'],
    ['toomanyrequests: You have reached your pull rate limit','publisher_rate_limited'],
    ['received unexpected HTTP status: 429 Too Many Requests','publisher_rate_limited'],
    ['unauthorized: authentication required','publisher_access_denied'],
    ['denied: requested access to the resource is denied','publisher_access_denied'],
    ['pull access denied for example, repository does not exist','publisher_access_denied'],
    ['failed to resolve reference: not found','publisher_manifest_unavailable'],
    ['x509: certificate signed by unknown authority','publisher_network_failed'],
    ['dial tcp: lookup registry.invalid: no such host','publisher_network_failed'],
    ['net/http: TLS handshake timeout','publisher_network_failed'],
    ['unexpected daemon response','publisher_pull_failed'],
    ['','publisher_pull_failed'],
    [undefined,'publisher_pull_failed'],
  ])assert.equal(classifyPullFailure(stderr),code);
});

test('Publisher pull classification reads only a bounded stderr tail',()=>{
  assert.equal(classifyPullFailure('x'.repeat(200000)+'\nError response from daemon: manifest unknown'),'publisher_manifest_unavailable');
  assert.equal(classifyPullFailure('unauthorized 404'.padEnd(5000,'x')+'\nunexpected response'),'publisher_pull_failed');
});

test('Publisher pull classification returns fixed codes for hostile stderr',()=>{
  assert.equal(classifyPullFailure(fakeURL+' '+fakePath+' Authorization: Bearer FAKE_TOKEN unauthorized'),'publisher_access_denied');
  assert.equal(classifyPullFailure(fakeURL+' '+fakePath),'publisher_pull_failed');
});

test('verifyPublisher checks the pinned pull and version through injected tools',async()=>{
  const pulls=[],versions=[];
  await verifyPublisher({
    execute:async(...args)=>{pulls.push(args);return {out:'',err:'',code:0};},
    runSkopeo:async(...args)=>{versions.push(args);return {out:pins.ci.publisherVersion+'\n'};},
  });
  assert.deepEqual(pulls,[[['pull',pins.ci.publisher],{timeout:300000,allowFailure:true}]]);
  assert.deepEqual(versions,[[['--version']]]);
});

test('verifyPublisher rejects a failed pull with a fixed code without echoing stderr',async()=>{
  const versions=[];
  await assert.rejects(verifyPublisher({
    execute:async()=>({out:'',err:fakeURL+' '+fakePath+' unauthorized: authentication required',code:1}),
    runSkopeo:async(...args)=>{versions.push(args);return {out:''};},
  }),err=>{
    assert.equal(err.message,'publisher_access_denied');
    for(const secret of [fakeCredential,fakePath,fakeURL,'FAKE_TOKEN','Bearer','registry.invalid'])assert.ok(!err.message.includes(secret));
    return true;
  });
  assert.equal(versions.length,0);
});

test('verifyPublisher maps a rejected docker call to the generic code without running skopeo',async()=>{
  const versions=[];
  await assert.rejects(verifyPublisher({
    execute:async()=>{throw new Error('subprocess_failed_docker_pull');},
    runSkopeo:async(...args)=>{versions.push(args);return {out:''};},
  }),{message:'publisher_pull_failed'});
  assert.equal(versions.length,0);
});

test('verifyPublisher still rejects a different publisher version after a clean pull',async()=>{
  await assert.rejects(verifyPublisher({
    execute:async()=>({out:'',err:'',code:0}),
    runSkopeo:async()=>({out:'skopeo version 0.0.0\n'}),
  }),{message:'publisher_version_mismatch'});
});

for (const architecture of ['amd64','arm64']) {
 test(`Skopeo ${architecture} pin retains digest-bound official publisher identity`,()=>{
  const entry=manifest.manifests.find(value=>value.platform.os==='linux'&&value.platform.architecture===architecture);
  assert.ok(entry);
  const imageRaw=readFileSync(new URL(`./fixtures/skopeo-v1.22.3-${architecture}-manifest.json`,import.meta.url));
  assert.equal('sha256:'+createHash('sha256').update(imageRaw).digest('hex'),entry.digest);
  assert.equal(imageRaw.length,entry.size);
  const image=JSON.parse(imageRaw);
  assert.equal(image.mediaType,'application/vnd.oci.image.manifest.v1+json');
  const configRaw=readFileSync(new URL(`./fixtures/skopeo-v1.22.3-${architecture}-config.json`,import.meta.url));
  assert.equal('sha256:'+createHash('sha256').update(configRaw).digest('hex'),image.config.digest);
  assert.equal(configRaw.length,image.config.size);
  const config=JSON.parse(configRaw);
  assert.equal(config.os,'linux');
  assert.equal(config.architecture,architecture);
  assert.deepEqual(config.config.Entrypoint,['/usr/bin/skopeo']);
  assert.equal(config.config.Labels['org.opencontainers.image.version'],'1.22.3');
  assert.equal(config.config.Labels['built.by.repo'],'https://github.com/podman-container-tools/image_build');
  assert.equal(config.config.Labels['org.opencontainers.image.revision'],'b86020d8c306c4724780adfb6fc492d0a2d9cc48');
  assert.equal(config.config.Labels['org.opencontainers.image.url'],'https://quay.io/skopeo/stable');
 });
}
