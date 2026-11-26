// Run with Node 22+ and a local checkout of the official alma-relay with its dependencies installed.
// node apps/service/tests/alma_interop.mjs target/debug/vantry-service /path/to/alma-relay
import assert from 'node:assert/strict';
import { once } from 'node:events';
import { deadline, waitFor, withAlmaFixture } from './alma_interop_client.mjs';

if (process.argv.length !== 4) throw new Error('Provide the service executable and official relay directory');
const shared = 'th_alma_test';
await withAlmaFixture(process.argv[2], process.argv[3], shared, async ({ctl, connect, restart, url, version}) => {
    await ctl('create','--thread','hidden','--title','Other');
    await ctl('send','--thread',shared,'--message','Desktop seed');
    await waitFor(async () => (await ctl('snapshot')).runs?.[0]?.status === 'completed');
    const path = '/api/threads/'+shared;
    assert.equal((await fetch(url+path)).status,403);
    const phone = await connect();
    assert.deepEqual((await phone.http('/api/threads')).body.map(t=>t.id),[shared]);
    assert.deepEqual((await phone.http('/api/threads?ids=hidden')).body,[]);
    assert.equal((await phone.http('/api/threads/hidden/messages')).status,404);
    assert.equal((await phone.http('/api/threads/hidden/send',{text:'Out of scope'})).status,404);
    const seed = (await phone.http(path+'/messages')).body;
    assert.equal(seed.length,2);
    assert.equal(seed[0].message.parts[0].text,'Desktop seed');
    for (const [depth,row] of seed.entries()) {
        assert.equal(row.metadata.slotId,row.slotId);
        assert.equal(row.metadata.depth,depth);
        assert.equal(row.metadata.versionIndex,0);
        assert.equal(row.metadata.versionCount,1);
        assert.equal(row.parentToolCallId,null);
    }
    const tail = (await phone.http(path+'?tail=1')).body;
    assert.equal(tail.messagesOffset,1);
    assert.equal(tail.messagesTotal,2);
    assert.deepEqual(tail.messages.map(m=>m.id),[seed[1].id]);
    phone.send({t:'wopen',id:'w1',path:'/ws/threads'});
    assert.equal((await phone.receive()).t,'wopened');
    const generate = {t:'wmsg',id:'w1',s:JSON.stringify({type:'generate_response',data:{
        threadId:shared,userMessage:{role:'user',parts:[{type:'text',text:'Phone input'}]},
        userMessageMetadata:{optimisticId:'isolated-operation'}}})};
    phone.send(generate);
    phone.send(generate);
    let streamed = '', pieces = 0, assistantId;
    for (;;) {
        const event = JSON.parse((await phone.receive()).s);
        if (event.type === 'message_delta') {
            assistantId ??= event.data.messageId;
            assert.equal(event.data.messageId,assistantId);
            streamed += event.data.deltas.map(d=>d.text).join('');
            pieces++;
        }
        if (event.type === 'thread_generating' && !event.data.isGenerating) break;
    }
    assert(pieces>1);
    phone.socket.close();
    const resumed = await connect();
    const history = (await resumed.http(path+'/messages')).body;
    assert.equal(history.length,4);
    assert.deepEqual(history.slice(0,2).map(m=>m.id),seed.map(m=>m.id));
    assert.equal(history[3].id,assistantId);
    assert.equal(history[2].metadata.optimisticId,'isolated-operation');
    assert.equal(history[0].metadata.optimisticId,undefined);
    assert.equal(history[3].metadata.optimisticId,undefined);
    assert.equal(history[3].message.parts[0].text,streamed);
    assert(streamed.includes('Phone input'));
    const page = (await resumed.http(path+'/messages?page=1&offset=2&limit=1')).body;
    assert.equal(page.messages[0].id,history[2].id);
    assert.equal((await resumed.http(path+'/send',{text:'Phone input'})).status,202);
    // Re-deliver one encrypted read frame to our host. It must end this session.
    const packet = resumed.send({t:'hreq',id:'last',path:'/api/threads',method:'GET'});
    const closed = once(resumed.socket,'close');
    resumed.socket.send(packet);
    await deadline(closed);
    await waitFor(async () => {
        const {runs} = await ctl('snapshot');
        assert(!runs?.some(r=>r.status==='failed' || r.status==='cancelled'),
            'Isolated worker failed: ' + runs?.filter(r=>r.error).map(r=>r.error).join(', '));
        return runs?.length === 3 && runs.every(r=>r.threadId===shared && r.status==='completed');
    }, 20000);
    await restart();
    const restarted = await connect();
    const saved = (await restarted.http(path+'/messages')).body;
    assert.equal(saved.length,6);
    assert.deepEqual(saved.slice(0,4).map(m=>m.id),history.map(m=>m.id));
    assert.equal(saved[2].metadata.optimisticId,'isolated-operation');
    restarted.send({t:'wopen',id:'w1',path:'/ws/threads'});
    assert.equal((await restarted.receive()).t,'wopened');
    restarted.send(generate);
    for (;;) {
        const event = JSON.parse((await restarted.receive()).s);
        if (event.type === 'message_added' && event.data.metadata?.optimisticId === 'isolated-operation') {
            assert.equal(event.data.id,history[2].id);
            break;
        }
    }
    assert.equal((await ctl('snapshot')).runs.length,3);
    console.log(`PASS ${version} with Alma registration, E2E, scoped history and writes, stream, reconnection, replay rejection and idempotency across a Core restart.`);
});
