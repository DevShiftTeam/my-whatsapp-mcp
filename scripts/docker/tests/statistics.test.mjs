import {test} from 'node:test';
import assert from 'node:assert/strict';
import {summarize} from '../statistics.mjs';
test('median averages both middle observations for even sample counts',()=>{assert.deepEqual(summarize([7,1,3,9]),{samples:[7,1,3,9],median:5,max:9});});
test('median uses middle observation for five samples',()=>{assert.equal(summarize([5,2,9,1,3]).median,3);});

import {fixtureMode,singleApplicationProcess} from '../lib.mjs';
test('benchmark mode preserves demo and selects normal serve without demo arguments',()=>{
 assert.deepEqual(fixtureMode('demo'),{mode:'demo',root:'/var/lib/wa-indexer/demo',healthArgs:['--demo']});
 assert.deepEqual(fixtureMode('serve'),{mode:'serve',root:'/var/lib/wa-indexer/data',healthArgs:[]});
 for(const value of ['normal','',null,'--demo'])assert.throws(()=>fixtureMode(value));
});

test('process proof requires a PID column and exactly one app process',()=>{
 assert.equal(singleApplicationProcess('PID COMMAND\n120 wa-indexer\n'),true);
 for(const value of ['COMMAND\nwa-indexer','PID COMMAND\n120 cloudflared','PID COMMAND\n120 wa-indexer\n121 cloudflared','PID COMMAND\n'])assert.equal(singleApplicationProcess(value),false);
});
