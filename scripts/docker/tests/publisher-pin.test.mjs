import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {pins} from '../oci-tools.mjs';

// Primary-registry index retrieved 2026-10-13 using an anonymous Registry v2
// pull token, then fetched by its digest. The retired 9ba3be83 digest returned
// authenticated 404 MANIFEST_UNKNOWN; the versioned v1.22.3 tag still exists.
const raw=readFileSync(new URL('./fixtures/skopeo-v1.22.3-index.json',import.meta.url));
const manifest=JSON.parse(raw);
const digest='sha256:'+createHash('sha256').update(raw).digest('hex');

test('Skopeo tool pin matches the reviewed available versioned native-platform index',()=>{
 assert.equal(manifest.schemaVersion,2);
 assert.equal(manifest.mediaType,'application/vnd.oci.image.index.v1+json');
 assert.deepEqual(manifest.manifests.map(entry=>entry.platform.os+'/'+entry.platform.architecture).sort(),['linux/amd64','linux/arm64','linux/ppc64le','linux/s390x']);
 for(const entry of manifest.manifests){assert.match(entry.digest,/^sha256:[a-f0-9]{64}$/);assert.ok(entry.size>0);}
 assert.equal(pins.ci.publisher,'quay.io/skopeo/stable:v1.22.3@'+digest);
 assert.equal(pins.ci.publisherVersion,'skopeo version 1.22.3 commit: 92397d5ba399784c6e2f9f229a4244b724743dba');
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
