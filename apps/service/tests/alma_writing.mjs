// Run with the same locally built service and relay as alma_interop.mjs. All generated text is synthetic.
import assert from 'node:assert/strict';
import {readFile, writeFile} from 'node:fs/promises';
import {join} from 'node:path';
import {waitFor, withAlmaFixture} from './alma_interop_client.mjs';

const thread = 'writing_fixture', provider = 'gemini-writing', model = provider + '/neon-high';
const prose = '潮水留下了细沙。\n\n'.repeat(375) + '尾灯熄灭。';
let audit;
const testConfig = {threadArgs:['--provider',provider,'--model',model], async setup({root,data}) {
    audit = join(root,'calls');
    const adapter = join(root,'writing.mjs');
    const cfg = {threadId:thread, delivery:'synthetic', node:process.execPath, adapter, audit};
    await writeFile(join(data,'gemini-writing.json'),JSON.stringify(cfg));
    await writeFile(adapter, [
        "import {readFileSync,writeFileSync,appendFileSync,existsSync} from 'node:fs';",
        "const cfg=JSON.parse(readFileSync(process.argv[2])), req=JSON.parse(readFileSync(0));",
        "appendFileSync(cfg.audit,JSON.stringify(req)+'\\n');",
        "const emit=v=>console.log(JSON.stringify(v));",
        "emit({kind:'submitted'}); emit({kind:'observing'});",
        "if (!req.observeOnly) await new Promise(resolve=>setTimeout(resolve,30000));",
        "if (!existsSync(cfg.audit+'.first-observe')) {writeFileSync(cfg.audit+'.first-observe','');emit({kind:'problem',code:'incomplete'});}",
        "else {emit({kind:'delivering'});emit({kind:'synthetic',text:"+JSON.stringify(prose)+"+(existsSync(cfg.audit+'.refresh')?'':' 后续建议')});}",
    ].join('\n'));
}};
await withAlmaFixture(process.argv[2],process.argv[3],thread,async ({ctl,connect,restart}) => {
    const phone = await connect(), base = '/api/threads/'+thread;
    const models = (await phone.http('/api/models')).body.data;
    assert.deepEqual(models.map(m=>m.id),[model]);
    assert.equal((await phone.http(base,{model:'vantry:other'},'PUT')).status,400);
    assert.equal((await phone.http(base+'/send',{text:'没有可重用操作 ID 的写作请求。'})).status,400);
    const input = {text:'只转发这句写作要求。',optimisticId:'physical-equivalent'};
    assert.equal((await phone.http(base+'/send',input)).status,202);
    assert.equal((await phone.http(base+'/send',input)).status,202);
    const calls = async()=> (await readFile(audit,'utf8')).trim().split('\n').map(s=>JSON.parse(s));
    await waitFor(async()=>{try{return (await calls()).length === 1;}catch{return false;}});
    await restart();
    await waitFor(async()=>(await ctl('snapshot')).runs[0].status==='interrupted');
    const resumed=await connect();
    const run=(await ctl('snapshot')).runs[0];
    const retrieve={text:'取回写作 '+run.id,optimisticId:'retrieve'};
    await resumed.http(base+'/send',retrieve);
    await resumed.http(base+'/send',retrieve);
    await waitFor(async()=>(await ctl('snapshot')).runs[0].status==='completed');
    const rows=(await resumed.http(base+'/messages')).body;
    assert.deepEqual(rows.map(r=>r.message.parts[0].text),[input.text,prose+' 后续建议']);
    assert.equal(rows[0].metadata.optimisticId,input.optimisticId);
    await writeFile(audit+'.refresh','');
    await resumed.http(base+'/send',{...retrieve,optimisticId:'refresh-completed'});
    await waitFor(async()=>{
        const [updated]=(await ctl('snapshot')).runs;
        return updated.status==='completed' && updated.output===prose;
    });
    const refreshed=(await resumed.http(base+'/messages')).body;
    assert.deepEqual(refreshed.map(r=>r.id),rows.map(r=>r.id));
    assert.deepEqual(refreshed.map(r=>r.message.parts[0].text),[input.text,prose]);
    assert.deepEqual((await ctl('snapshot')).blocks.filter(b=>b.kind==='text').map(b=>b.text),[prose]);
    const seen=await calls();
    assert.deepEqual(seen.map(r=>r.observeOnly),[false,true,true,true]);
    assert(seen.every(r=>r.message===input.text && r.run===run.id && r.operation.endsWith('/op/'+input.optimisticId)));
    assert.equal((await ctl('snapshot')).runs.length,1);
    console.log('PASS: one writing operation across duplicate mobile sends, Core restart, interrupted observation, and explicit retrieval.');
},testConfig);
