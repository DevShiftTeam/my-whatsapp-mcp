import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {pins} from '../oci-tools.mjs';

// Anonymous primary-registry index retrieved 2026-10-12 from
// https://quay.io/v2/skopeo/stable/manifests/v1.22.3, then fetched by its digest.
// The retired e5d5d281 digest returned 404 MANIFEST_UNKNOWN on that date.
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
