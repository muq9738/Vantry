// Exercises the desktop's pairing actions against a normal Core with no Alma flags.
import assert from 'node:assert/strict';
import {once} from 'node:events';
import {mkdir, writeFile} from 'node:fs/promises';
import {join} from 'node:path';
import {createServer} from 'node:http';
import {deadline, routing, waitFor, withAlmaFixture} from './alma_interop_client.mjs';

const thread = 'studio-selected-chat';
const requestedModels = [];
const modelApi = createServer(async (req,res) => {
    const chunks = [];
    for await (const part of req) chunks.push(part);
    const {model} = JSON.parse(Buffer.concat(chunks));
    requestedModels.push(model);
    res.writeHead(200,{'content-type':'text/event-stream'});
    const frame = (delta, finish) => 'data: ' + JSON.stringify({id:'fixture',object:'chat.completion.chunk',
        created:1,model,choices:[{index:0,delta,finish_reason:finish}]}) + '\n\n';
    res.end(frame({role:'assistant',content:'Chosen local model.'},null) + frame({},'stop') + 'data: [DONE]\n\n');
});
await withAlmaFixture(process.argv[2], process.argv[3], thread, async fixture => {
    const {ctl, connectTo, restart, relay, registrationKey} = fixture;
    const action = async (name, extra = {}) => {
        const request = {action:'remote.pair.alma.' + name, threadId:thread, ...extra};
        const res = await ctl('workbench', JSON.stringify(request));
        return JSON.parse(res.output);
    };
    const connectLink = link => {
        const parts = new URL(link).searchParams;
        const key = parts.get('k');
        assert(key && parts.get('e2e') === '1');
        return connectTo(parts.get('u') + '/c/' + routing(key), key);
    };
    assert.equal((await action('status')).enabled, false);
    await ctl('model','--thread',thread,'--model','selected-thread-model','--provider','controlled');
    await ctl('create', '--thread', 'unshared-thread', '--title', 'Other chat');
    const created = await action('create');
    assert(created.enabled && created.ready && created.link && !created.relay);
    assert.equal(created.phones, 0);
    assert(!(await action('status')).link);
    assert(!(await action('show',{threadId:'unshared-thread'})).link);
    assert((await action('create')).link === created.link);
    await assert.rejects(action('create',{threadId:'unshared-thread'}));
    const phone = await connectLink(created.link);
    // Alma 1.69 cancels its initial load if a sibling of the thread list fails.
    const boot = {};
    for (const path of ['settings','models','threads?fields=list','workspaces','thread-labels',
        'boards','personal','personal/runs?limit=50','herdr/info','herdr/mutes','herdr/pins',
        'update/status','alma/vitals','handoff/user-questions','health']) {
        const response = await phone.http('/api/' + path);
        assert.equal(response.status,200,path);
        boot[path] = response.body;
    }
    const threads = boot['threads?fields=list'];
    assert.deepEqual(threads.map(t=>t.id), [thread]);
    assert.deepEqual(boot.workspaces.map(w=>w.id),[threads[0].workspaceId]);
    assert.equal(boot.settings.chat.defaultModel,threads[0].model);
    const choices = boot.models.data.map(m=>m.providerId+':'+m.id);
    assert(choices.includes(threads[0].model));
    assert(choices.includes('vantry:fixture/second'));
    assert.equal(boot.models.data[0].id,'selected-thread-model');
    assert.equal(boot.personal.threadId,thread);
    assert.equal(boot.personal.settings.threadId,thread);
    assert.equal((await phone.http('/api/threads/'+boot.personal.threadId+'?tail=30')).body.id,thread);
    assert.equal((await phone.http('/api/threads/unshared-thread/file-writes')).status,404);
    assert.deepEqual(boot['thread-labels'],[]);
    assert.deepEqual(boot.boards.boards,[]);
    assert.deepEqual(boot['personal/runs?limit=50'].runs,[]);
    assert.equal(boot['herdr/info'].online,false);
    assert.equal((await phone.http('/api/settings',{chat:{defaultModel:'different'}})).status,404);
    assert.equal((await phone.http('/api/providers')).status,404);
    phone.send({t:'wopen',id:'questions',path:'/ws/handoff/user-questions'});
    assert.equal((await phone.receive()).t,'wopened');
    phone.send({t:'wclose',id:'questions'});
    phone.send({t:'wopen',id:'presence',path:'/ws/herdr?delta=1'});
    assert.equal((await phone.receive()).t,'wopened');
    for (const type of ['herdr','pins','archived']) {
        const frame = await phone.receive();
        assert.equal(frame.id,'presence');
        assert.equal(JSON.parse(frame.s).type,type);
    }
    assert.equal((await phone.http('/api/threads/unshared-thread')).status,404);
    assert.equal((await action('status')).phones,1);
    const lan = (await phone.http('/__alma_lan__')).body;
    assert(lan.hosts.length && lan.port);
    const direct = await connectTo('http://' + lan.hosts[0] + ':' + lan.port, new URL(created.link).searchParams.get('k'));
    assert.deepEqual((await direct.http('/api/threads')).body.map(t=>t.id),[thread]);
    direct.socket.close();
    phone.send({t:'wopen',id:'chat',path:'/ws/threads'});
    assert.equal((await phone.receive()).t,'wopened');
    const message = '从手机继续选中的 Vantry 会话';
    phone.send({t:'wmsg',id:'chat',s:JSON.stringify({type:'generate_response',data:{
        threadId:thread,userMessage:{role:'user',parts:[{type:'text',text:message}]},
        userMessageMetadata:{optimisticId:'from-device'},
    }})});
    let streamed = '', deltas = 0;
    for (;;) {
        const event = JSON.parse((await phone.receive()).s);
        if (event.type === 'generation_error') assert.fail('Selected thread rejected message');
        if (event.type === 'message_delta') {
            streamed += event.data.deltas.map(d=>d.text).join('');
            deltas++;
        }
        if (event.type === 'thread_generating' && !event.data.isGenerating) break;
    }
    const rows = (await ctl('snapshot')).runs;
    assert.equal(rows.length,1);
    assert.equal(rows[0].model, 'selected-thread-model');
    assert.equal(rows[0].providerId, 'controlled');
    assert(rows[0].threadId === thread && rows[0].input === message && rows[0].output === streamed && deltas > 1);
    await restart();
    assert((await action('show')).link === created.link);
    const resumed = await connectLink(created.link);
    const history = (await resumed.http('/api/threads/' + thread + '/messages')).body;
    assert.equal(history[0].metadata.optimisticId, 'from-device');
    assert.equal(history[1].message.parts[0].text, streamed);
    const change = (name, model) => resumed.http('/api/threads/'+name, {model,skipUpdatedAt:true}, 'PUT');
    assert.equal((await change('unshared-thread','vantry:fixture/second')).status,404);
    assert.equal((await change(thread,'vantry:missing')).status,400);
    assert.equal((await change(thread,'vantry:fixture/second')).body.model,'vantry:fixture/second');
    const selected = (await ctl('snapshot')).threads.find(t => t.id === thread);
    assert.equal(selected.model, 'fixture/second');
    assert.equal(selected.providerId, 'pi');
    assert.equal((await resumed.http('/api/threads/'+thread)).body.model,'vantry:fixture/second');
    assert.equal((await resumed.http('/api/threads/'+thread+'/send',
        {text:'Model selection test',optimisticId:'chosen'})).status,202);
    await waitFor(async () => {
        const run = (await ctl('snapshot')).runs.find(r=>r.input==='Model selection test');
        assert(!run?.error,run?.error);
        return run?.status === 'completed' && run.output === 'Chosen local model.';
    },20000);
    assert.deepEqual(requestedModels,['second']);
    const closed = once(resumed.socket,'close');
    assert.equal((await action('revoke',{name:created.shareId})).enabled,false);
    await deadline(closed);
    await assert.rejects(connectLink(created.link));
    await restart();
    assert.equal((await action('status')).enabled,false);
    const remote = await action('create',{threadId:'unshared-thread', input:{relay,registrationKey}});
    assert(remote.link !== created.link);
    await waitFor(async () => (await action('status')).ready);
    const viaRelay = await connectLink(remote.link);
    assert.deepEqual((await viaRelay.http('/api/threads')).body.map(t=>t.id),['unshared-thread']);
    assert(!(await action('status')).link);
    await action('revoke',{name:remote.shareId});
    console.log('PASS ' + fixture.version + ': desktop-created QR, LAN and relay, scoped chat, ' + deltas
        + ' streaming deltas, existing pairing after restart, and immediate revocation.');
}, {cliHost:false, async setup({root}) {
    modelApi.listen(0,'127.0.0.1');
    await once(modelApi,'listening');
    const dir = join(root,'pi');
    await mkdir(dir);
    await writeFile(join(dir,'models.json'),JSON.stringify({providers:{fixture:{baseUrl:'http://127.0.0.1:'+modelApi.address().port+'/v1',
        api:'openai-completions',apiKey:'local-fixture',models:[{id:'first',name:'First'},{id:'second',name:'Second'}]}}}));
    return {PI_CODING_AGENT_DIR:dir};
}}).finally(()=>new Promise(resolve=>modelApi.close(resolve)));
