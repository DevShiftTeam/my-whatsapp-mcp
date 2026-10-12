import {test} from 'node:test';
import assert from 'node:assert/strict';
import {api} from '../lib.mjs';

function fixture(t,{localCapability='synthetic-native-capability',sessionCapability=null,cookie=true}={}){
 const originals=new Map(['localStorage','sessionStorage','fetch'].map(key=>[key,Object.getOwnPropertyDescriptor(globalThis,key)]));
 t.after(()=>{for(const [key,descriptor] of originals){if(descriptor)Object.defineProperty(globalThis,key,descriptor);else delete globalThis[key];}});
 const local=new Map(localCapability?[['wa_archive_owner_capability',localCapability]]:[]);
 const session=new Map(sessionCapability?[['wa_archive_owner_capability',sessionCapability]]:[]);
 const requests=[];
 for(const [key,value] of Object.entries({localStorage:{getItem:key=>local.get(key)||null},sessionStorage:{getItem:key=>session.get(key)||null},fetch:async(path,options)=>{
  requests.push({path,...options});const status=cookie&&options.headers['X-CSRF-Token']==='synthetic-native-capability'?200:403;
  return {status,json:async()=>({authorized:status===200})};
 }}))Object.defineProperty(globalThis,key,{value,configurable:true,writable:true});
 return {page:{evaluate:(callback,args)=>callback(args)},local,requests};
}

test('native bootstrap cookie and localStorage-only capability produce authenticated owner API requests',async t=>{
 const f=fixture(t);
 const result=await api(f.page,'/api/remote/status');
 assert.equal(result.status,200);assert.equal(result.value.authorized,true);
 assert.equal(f.requests[0].headers['X-CSRF-Token'],'synthetic-native-capability');
 assert.equal(f.requests[0].body,undefined);
});

test('stale donor sessionStorage cannot override the main native capability or mutate request data',async t=>{
 const f=fixture(t,{sessionCapability:'synthetic-stale-donor-capability'});
 const body={synthetic:true};const result=await api(f.page,'/api/search','POST',body);
 assert.equal(result.status,200);assert.equal(f.requests[0].headers['X-CSRF-Token'],'synthetic-native-capability');
 assert.equal(f.requests[0].method,'POST');assert.equal(f.requests[0].body,JSON.stringify(body));assert.deepEqual(body,{synthetic:true});
});

test('missing or removed native capability stays denied even if stale sessionStorage remains',async t=>{
 const f=fixture(t,{sessionCapability:'synthetic-stale-donor-capability'});
 assert.equal((await api(f.page,'/api/setup')).status,200);
 f.local.delete('wa_archive_owner_capability');
 assert.equal((await api(f.page,'/api/setup')).status,403);
 assert.equal(f.requests.at(-1).headers['X-CSRF-Token'],'');
});

test('native capability alone does not replace the owner cookie',async t=>{
 const f=fixture(t,{cookie:false});
 assert.equal((await api(f.page,'/api/setup')).status,403);
});
