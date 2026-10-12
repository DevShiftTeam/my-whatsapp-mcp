// Validate the complete transferable graph in memory. Never extract an untrusted tar.
import {createHash} from 'node:crypto';
import {readFileSync,readdirSync,lstatSync} from 'node:fs';
import {join} from 'node:path';
import {gunzipSync} from 'node:zlib';
import {check} from './lib.mjs';

export const digest=bytes=>'sha256:'+createHash('sha256').update(bytes).digest('hex');
const indexType='application/vnd.oci.image.index.v1+json';
const manifestType='application/vnd.oci.image.manifest.v1+json';
const source='https://github.com/DevShiftTeam/my-whatsapp-mcp';
const json=bytes=>JSON.parse(bytes.toString('utf8'));

export function readArchive(path) {
  const archive=readFileSync(path), files=new Map();
  check(archive.length<512*1024*1024,'oci_archive_too_large');
  check(archive.length%512===0,'tar_truncated');let ended=false;
  for(let offset=0;offset+512<=archive.length;) {
    const h=archive.subarray(offset,offset+512);offset+=512;
    if(h.every(b=>b===0)){check(offset+512<=archive.length&&archive.subarray(offset).every(b=>b===0),'tar_trailing_payload');ended=true;break;}
    const str=(a,b)=>h.subarray(a,b).toString('utf8').replace(/\0.*$/s,'');
    const octal=(a,b)=>{const s=str(a,b).trim();check(/^[0-7]+$/.test(s),'tar_invalid_number');return parseInt(s,8);};
    const sum=[...h].reduce((s,b,i)=>s+(i>=148&&i<156?32:b),0);
    check(sum===octal(148,156),'tar_header_checksum');
    const prefix=str(345,500), name=(prefix?prefix+'/':'')+str(0,100), size=octal(124,136), type=str(156,157);
    check(Number.isSafeInteger(size)&&offset+size<=archive.length,'tar_truncated');
    check(!name.startsWith('/')&&!name.split('/').includes('..')&&!name.includes('\\'),'tar_unsafe_path');
    if(type==='5')check(size===0,'tar_directory_payload');
    else {
      check(type==='0'||type==='','tar_nonregular_entry');
      check(!files.has(name),'tar_duplicate_entry');files.set(name,archive.subarray(offset,offset+size));
    }
    offset+=Math.ceil(size/512)*512;
  }
  check(ended,'tar_end_missing');return {files,archiveSHA256:digest(archive)};
}

export function inspectLayout(files,{platform,sourceRevision,imageID}={}) {
  check(json(files.get('oci-layout')||Buffer.from('{}')).imageLayoutVersion==='1.0.0','oci_layout_version');
  const topBytes=files.get('index.json');check(topBytes,'oci_index_missing');const top=json(topBytes);
  const visited=new Map();
  function blob(d) {
    check(d&&/^sha256:[a-f0-9]{64}$/.test(d.digest)&&Number.isSafeInteger(d.size)&&d.size>=0,'oci_descriptor_invalid');
    const bytes=files.get('blobs/sha256/'+d.digest.slice(7));
    check(bytes&&bytes.length===d.size&&digest(bytes)===d.digest,'oci_blob_integrity');
    visited.set(d.digest,{digest:d.digest,size:d.size,mediaType:d.mediaType});return bytes;
  }
  const manifests=[];
  function walk(d,depth=0) {
    check(depth<5,'oci_index_depth');const bytes=blob(d);
    if(d.mediaType===indexType){const i=json(bytes);check(i.schemaVersion===2&&Array.isArray(i.manifests),'oci_index_invalid');for(const child of i.manifests)walk(child,depth+1);}
    else {
      check(d.mediaType===manifestType,'oci_manifest_type');const m=json(bytes);
      check(m.schemaVersion===2&&Array.isArray(m.layers),'oci_manifest_invalid');
      const config=json(blob(m.config));const layers=m.layers.map(l=>({descriptor:l,bytes:blob(l)}));
      manifests.push({descriptor:d,configDescriptor:m.config,config,layers});
    }
  }
  check(top.schemaVersion===2&&Array.isArray(top.manifests)&&top.manifests.length>0,'oci_top_index_invalid');
  top.manifests.forEach(d=>walk(d));
  const runnables=manifests.filter(m=>m.descriptor.platform?.os==='linux');
  check(runnables.length>0&&runnables.length<=2,'oci_runnable_count');
  check(manifests.length===runnables.length*2,'oci_attestation_count');
  const platforms=runnables.map(m=>{
    const p=m.descriptor.platform.os+'/'+m.descriptor.platform.architecture;
    check(['linux/amd64','linux/arm64'].includes(p),'oci_platform');
    check(m.config.os+'/'+m.config.architecture===p,'oci_config_platform');
    if(platform)check(p===platform,'oci_expected_platform');
    if(imageID)check(m.configDescriptor.digest===imageID,'oci_tested_config_mismatch');
    const labels=m.config.config?.Labels;
    check(/^[a-f0-9]{40}$/.test(labels?.['org.opencontainers.image.revision']),'oci_revision_missing');
    if(sourceRevision)check(labels['org.opencontainers.image.revision']===sourceRevision,'oci_source_mismatch');
    check(labels['org.opencontainers.image.source']===source&&labels['org.opencontainers.image.licenses']==='GPL-3.0-only','oci_source_license_labels');
    check(m.config.rootfs?.diff_ids?.length===m.layers.length,'oci_diff_ids_count');
    m.layers.forEach((l,i)=>{
      check(['application/vnd.oci.image.layer.v1.tar+gzip','application/vnd.oci.image.layer.v1.tar'].includes(l.descriptor.mediaType),'oci_layer_compression');
      const uncompressed=l.descriptor.mediaType.endsWith('+gzip')?gunzipSync(l.bytes,{maxOutputLength:512*1024*1024}):l.bytes;
      check(digest(uncompressed)===m.config.rootfs.diff_ids[i],'oci_layer_diff_id');
    });
    const attestations=manifests.filter(a=>a.descriptor.annotations?.['vnd.docker.reference.type']==='attestation-manifest'&&a.descriptor.annotations?.['vnd.docker.reference.digest']===m.descriptor.digest);
    check(attestations.length===1,'oci_attestation_link');
    const predicates=[];
    for(const a of attestations) {
      check(a.descriptor.platform?.os==='unknown'&&a.descriptor.platform?.architecture==='unknown','oci_attestation_platform');
      for(const l of a.layers){check(l.descriptor.mediaType==='application/vnd.in-toto+json','oci_attestation_layer_type');const s=json(l.bytes);
        check(['https://in-toto.io/Statement/v0.1','https://in-toto.io/Statement/v1'].includes(s._type)&&s.predicate&&s.subject?.some(x=>x.digest?.sha256===m.descriptor.digest.slice(7)),'oci_attestation_subject');
        if(s.predicateType==='https://slsa.dev/provenance/v1'){
          const args=s.predicate.buildDefinition?.externalParameters?.request?.args;
          check(args?.['build-arg:SOURCE_REVISION']===labels['org.opencontainers.image.revision']&&args.target==='runtime','oci_provenance_build_source');
        }
        if(s.predicateType==='https://spdx.dev/Document')check(/^SPDX-2\./.test(s.predicate.spdxVersion)&&typeof s.predicate.documentNamespace==='string'&&s.predicate.packages?.length>0,'oci_sbom_empty');
        predicates.push(s.predicateType);
      }
    }
    check(predicates.includes('https://slsa.dev/provenance/v1')&&predicates.includes('https://spdx.dev/Document'),'oci_provenance_sbom_required');
    return {platform:p,manifestDigest:m.descriptor.digest,configDigest:m.configDescriptor.digest,sourceRevision:labels['org.opencontainers.image.revision'],attestations:attestations.map(a=>a.descriptor.digest),predicates};
  }).sort((a,b)=>a.platform.localeCompare(b.platform));
  check(new Set(platforms.map(p=>p.platform)).size===platforms.length,'oci_duplicate_platform');
  for(const name of files.keys())check(name==='oci-layout'||name==='index.json'||/^blobs\/sha256\/[a-f0-9]{64}$/.test(name)&&visited.has('sha256:'+name.slice(13)),'oci_unreferenced_payload');
  const rootDigest=top.manifests.length===1&&top.manifests[0].mediaType===indexType?top.manifests[0].digest:digest(topBytes);
  return {rootDigest,platforms,graph:[...visited.values()].sort((a,b)=>a.digest.localeCompare(b.digest))};
}

export function inspectArchive(path,expected) {
  const {files,archiveSHA256}=readArchive(path);return {...inspectLayout(files,expected),archiveSHA256};
}

export function inspectDirectory(path,expected) {
  const files=new Map();
  function visit(relative='') {
    for(const name of readdirSync(join(path,relative))) {
      const rel=relative?relative+'/'+name:name, file=join(path,rel), stat=lstatSync(file);
      if(stat.isDirectory()){check(rel==='blobs'||rel==='blobs/sha256','oci_unexpected_directory');visit(rel);}
      else {check(stat.isFile()&&stat.size<512*1024*1024,'oci_nonregular_file');files.set(rel,readFileSync(file));}
    }
  }
  visit();return inspectLayout(files,expected);
}

export function verifyMergedPlatforms(graph,qualified) {
  const expected=qualified.flatMap(g=>g.platforms).sort((a,b)=>a.platform.localeCompare(b.platform));
  check(expected.length===2&&expected[0].platform==='linux/amd64'&&expected[1].platform==='linux/arm64','merge_requires_both_native_platforms');
  check(JSON.stringify(graph.platforms)===JSON.stringify(expected),'merged_graph_identity_mismatch');
}
