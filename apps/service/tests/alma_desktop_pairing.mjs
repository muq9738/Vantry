// Exercises the desktop's pairing actions against a normal Core with no Alma flags.
import assert from 'node:assert/strict';
import {once} from 'node:events';
import {mkdir, writeFile} from 'node:fs/promises';
import {join} from 'node:path';
import {createServer} from 'node:http';
import {deadline, routing, waitFor, withAlmaFixture} from './alma_interop_client.mjs';

const thread = 'studio-selected-chat';
const requests = [];
const regularModels = ['gemini-4-argon','gemini-4-hydrogen','gemini-3.8-flash','gemini-4-carbon','gemini-4-neon']
    .map(id => 'dmit-ai/group/' + id);
const webModel = 'gemini-web-bridge/gemini-web-neon-high';
const messageText = m => typeof m.content === 'string' ? m.content : m.content.map(p=>p.text ?? '').join('');
const modelApi = createServer(async (req,res) => {
    const chunks = [];
    for await (const part of req) chunks.push(part);
    const request = JSON.parse(Buffer.concat(chunks));
    requests.push(request);
    const {model} = request;
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
        // iOS replaces the relay path with its absolute client route.
        const route = new URL('/c/' + routing(key), parts.get('u'));
        return connectTo(route.toString(), key);
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
    assert.equal(new URL(created.link).searchParams.get('name'),'vantry');
    assert.equal(boot.settings.deviceName,'vantry');
    assert.equal(boot.health.name,'vantry');
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
    assert.deepEqual(requests.map(r=>r.model),['second']);
    const choose = async id => {
        const model = 'vantry:magpie/' + id;
        assert.equal((await change(thread,model)).body.model,model);
    };
    const generate = async (text, optimisticId) => {
        assert.equal((await resumed.http('/api/threads/'+thread+'/send',
            {text,optimisticId})).status,202);
        await waitFor(async () => {
            const run = (await ctl('snapshot')).runs.find(r=>r.input===text);
            assert(!run?.error,run?.error);
            return run?.status === 'completed' && run.output === 'Chosen local model.';
        },20000);
        return requests.at(-1);
    };
    let previous = 'Model selection test';
    for (const id of regularModels) {
        await choose(id);
        const text = '普通 Gemini 会话 ' + id;
        const request = await generate(text,'regular-'+id.split('/').at(-1));
        assert.equal(request.model,id);
        assert(request.messages.some(m=>['system','developer'].includes(m.role) && messageText(m).length > 0));
        assert(request.tools?.length > 0);
        assert(request.messages.some(m=>m.role==='user' && messageText(m)===previous));
        assert.equal(messageText(request.messages.at(-1)),text);
        previous = text;
    }
    await choose(webModel);
    const turns = ['  保留标点、换行和末尾空格。\n\n“原文”  ', '\t第二次只发这一句。\n'];
    for (const [n,text] of turns.entries()) {
        const request = await generate(text,'web-'+n);
        assert.equal(request.model,webModel);
        assert.deepEqual(request.messages,[{role:'user',content:text}]);
        assert(!request.tools?.length && !request.system && !request.instructions);
    }
    const submitted = 1 + regularModels.length + turns.length;
    assert.equal(requests.length,submitted);
    const replace = (id, key) => action('replace',{name:id,input:{relay,registrationKey:key}});
    await assert.rejects(replace(created.shareId,'invalid-fixture-key'));
    assert.equal((await action('show')).link,created.link);
    assert.equal((await resumed.http('/api/threads')).status,200);
    const closed = once(resumed.socket,'close');
    const fresh = await replace(created.shareId,registrationKey);
    assert(fresh.ready && fresh.shareId !== created.shareId);
    assert.notEqual(new URL(fresh.link).searchParams.get('k'),new URL(created.link).searchParams.get('k'));
    await deadline(closed);
    await assert.rejects(connectLink(created.link));
    await assert.rejects(replace(created.shareId,registrationKey));
    await restart();
    assert.equal((await action('show')).link,fresh.link);
    const freshPhone = await connectLink(fresh.link);
    const saved = (await freshPhone.http('/api/threads/'+thread+'/messages')).body;
    assert.equal(saved[0].metadata.optimisticId,'from-device');
    assert.equal((await freshPhone.http('/api/threads/'+thread+'/send',
        {text:turns[1],optimisticId:'web-1'})).status,202);
    assert.equal((await ctl('snapshot')).runs.length,1+submitted);
    assert.equal(requests.length,submitted);
    assert((await freshPhone.http('/api/models')).body.data.some(m=>m.id==='magpie/'+webModel));
    const disconnected = once(freshPhone.socket,'close');
    assert.equal((await action('revoke',{name:fresh.shareId})).enabled,false);
    await deadline(disconnected);
    await assert.rejects(connectLink(fresh.link));
    await restart();
    assert.equal((await action('status')).enabled,false);
    const remote = await action('create',{threadId:'unshared-thread', input:{relay,registrationKey}});
    assert(remote.link !== created.link);
    await waitFor(async () => (await action('status')).ready);
    const viaRelay = await connectLink(remote.link);
    assert.deepEqual((await viaRelay.http('/api/threads')).body.map(t=>t.id),['unshared-thread']);
    const cachedLan = (await viaRelay.http('/__alma_lan__')).body;
    const mobileCode = new URL(remote.link).searchParams.get('k');
    await fixture.stopRelay();
    await waitFor(async () => !(await action('status')).ready);
    const onWifi = await connectTo('http://' + cachedLan.hosts[0] + ':' + cachedLan.port, mobileCode);
    assert.deepEqual((await onWifi.http('/api/threads')).body.map(t=>t.id),['unshared-thread']);
    fixture.resumeRelay();
    await waitFor(async () => (await action('status')).ready);
    const outside = await connectLink(remote.link);
    assert.deepEqual((await outside.http('/api/threads')).body.map(t=>t.id),['unshared-thread']);
    assert(!(await action('status')).link);
    await action('revoke',{name:remote.shareId});
    const titleThread = 'untitled-gemini-web', titleText = '  第一段文字。\n第二段仍由用户书写。  ';
    await ctl('create','--thread',titleThread,'--provider','pi','--model','magpie/'+webModel);
    await ctl('send','--thread',titleThread,'--message',titleText);
    await waitFor(async () => {
        const snapshot = await ctl('snapshot');
        return snapshot.threads.find(t=>t.id===titleThread)?.title === '第一段文字' &&
            snapshot.runs.find(r=>r.threadId===titleThread)?.status === 'completed';
    },20000);
    assert.equal(requests.length,submitted+1,'Gemini Web titles must stay local');
    assert.deepEqual(requests.at(-1).messages,[{role:'user',content:titleText}]);
    const regularTitle = 'untitled-regular-gemini';
    await ctl('create','--thread',regularTitle,'--provider','pi','--model','magpie/'+regularModels.at(-1));
    await ctl('send','--thread',regularTitle,'--message',titleText);
    await waitFor(async () => {
        const snapshot = await ctl('snapshot');
        return snapshot.threads.find(t=>t.id===regularTitle)?.title &&
            snapshot.runs.find(r=>r.threadId===regularTitle)?.status === 'completed';
    },20000);
    const regularRequests = requests.slice(submitted+1);
    assert.equal(regularRequests.length,2,'ordinary Gemini uses the standard title and agent requests');
    assert(regularRequests.some(r=>r.messages.some(m=>['system','developer'].includes(m.role) &&
        messageText(m).includes('Name this conversation'))));
    console.log('PASS ' + fixture.version + ': desktop-created QR, LAN and relay, scoped chat, ' + deltas
        + ' streaming deltas, five standard Gemini routes, verbatim Gemini Web, atomic re-pairing, restart, and immediate revocation.');
}, {cliHost:false, async setup({root}) {
    modelApi.listen(0,'127.0.0.1');
    await once(modelApi,'listening');
    const dir = join(root,'pi');
    await mkdir(dir);
    const localApi = {baseUrl:'http://127.0.0.1:'+modelApi.address().port+'/v1',api:'openai-completions',apiKey:'local-fixture'};
    await writeFile(join(dir,'models.json'),JSON.stringify({providers:{
        fixture:{...localApi,models:[{id:'first',name:'First'},{id:'second',name:'Second'}]},
        magpie:{...localApi,models:[...regularModels,webModel].map(id=>({id,name:id}))},
    }}));
    return {PI_CODING_AGENT_DIR:dir};
}}).finally(()=>new Promise(resolve=>modelApi.close(resolve)));
