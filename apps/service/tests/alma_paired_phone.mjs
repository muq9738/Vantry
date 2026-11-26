// Manual test through an iPhone's existing Alma desktop pairing.
// node apps/service/tests/alma_paired_phone.mjs target/debug/vantry-service /path/to/alma-relay
import assert from 'node:assert/strict';
import { randomBytes, randomInt, randomUUID } from 'node:crypto';
import { once } from 'node:events';
import { createServer } from 'node:http';
import { waitFor, withAlmaFixture } from './alma_interop_client.mjs';

const alma = 'http://127.0.0.1:23001';
const textOf = message => (message?.parts ?? []).filter(p => p.type === 'text').map(p => p.text).join('');

async function api(path, method = 'GET', body) {
    const response = await fetch(alma + path, {
        method, signal: AbortSignal.timeout(7000),
        headers: body === undefined ? {} : {'content-type':'application/json'},
        body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!response.ok) throw new Error('Alma ' + method + ' ' + path.split('?')[0] + ' returned HTTP ' + response.status);
    return response.status === 204 ? {} : response.json();
}

async function generate({connect, ctl, thread}, marker, operation, onText) {
    const phone = await connect();
    try {
        phone.send({t:'wopen', id:'chat', path:'/ws/threads'});
        assert.equal((await phone.receive()).t, 'wopened');
        phone.send({t:'wmsg', id:'chat', s:JSON.stringify({type:'generate_response', data:{
            threadId:thread, userMessage:{role:'user', parts:[{type:'text', text:marker}]},
            userMessageMetadata:{optimisticId:operation},
        }})});
        let runId, output = '', deltas = 0;
        for (;;) {
            const frame = await phone.receive();
            if (frame.t !== 'wmsg') continue;
            const {type, data} = JSON.parse(frame.s);
            if (type === 'generation_error') throw new Error('Vantry rejected the diagnostic message');
            if (type === 'message_added' && data.metadata?.optimisticId === operation) {
                assert(data.id.endsWith(':user'));
                runId = data.id.slice(0,-5);
            }
            if (!runId) continue;
            if (type === 'message_delta' && data.messageId === runId) {
                for (const part of data.deltas) {
                    output += part.text;
                    deltas++;
                    onText(part.text);
                }
            }
            if (type === 'message_updated' && data.id === runId) {
                assert.equal(textOf(data.message), output);
                break;
            }
        }
        const run = (await ctl('snapshot')).runs.find(r => r.id === runId);
        assert.equal(run?.threadId, thread);
        assert.equal(run.status, 'completed');
        assert.equal(run.input, marker);
        assert.equal(run.output, output);
        assert(deltas > 1 && output.includes(marker));
        return {output, runId, deltas};
    } finally { phone.socket.close(); }
}

function json(response, status, data) {
    response.writeHead(status, {'content-type':'application/json'});
    response.end(JSON.stringify(data));
}

async function probe(fixture, suffix, desktopOnly) {
    const {signal} = fixture;
    const title = fixture.version.replace('vantry-service', 'Vantry') + (desktopOnly ? ' 桌面联调 ' : ' 手机联调 ') + suffix;
    const model = 'vantry-controlled-' + suffix;
    const desktop = 'VT-D-' + suffix;
    const mobile = 'VT-M-' + suffix;
    const password = randomBytes(24).toString('base64url');
    const runs = new Map();
    let providerId, threadId;
    const history = async () => (await api('/api/threads/' + encodeURIComponent(threadId) + '?tail=12')).messages ?? [];

    async function handle(req, res) {
        if (req.method === 'GET' && req.url === '/v1/models') {
            return json(res, 200, {object:'list', data:[{id:model, object:'model', owned_by:'local'}]});
        }
        if (req.method !== 'POST' || req.url !== '/v1/chat/completions') return json(res, 404, {});
        if (req.headers.authorization !== 'Bearer ' + password) return json(res, 401, {});
        const buffers = [];
        let length = 0;
        for await (const buffer of req) {
            length += buffer.length;
            if (length > 16 * 1024 * 1024) return json(res, 413, {});
            buffers.push(buffer);
        }
        const body = JSON.parse(Buffer.concat(buffers).toString());
        if (body.model !== model) return json(res, 400, {});
        const last = body.messages?.findLast(m => m.role === 'user')?.content;
        const prompt = typeof last === 'string' ? last : Array.isArray(last) ? last.filter(p => p.type === 'text').map(p => p.text).join('\n') : '';
        const marker = [desktop, mobile].find(word => prompt.includes(word));
        // Alma may issue auxiliary calls using the same provider. Only a marker
        // already saved as a user message in the owned thread can dispatch a run.
        const user = marker && threadId && (await history()).findLast(r => r.message?.role === 'user' && textOf(r.message).trim() === marker);
        const header = {id:'chatcmpl-' + randomUUID(), model, created:Math.floor(Date.now()/1000)};
        const chunk = (delta, reason = null) => {
            if (!res.destroyed) res.write('data: ' + JSON.stringify({...header, object:'chat.completion.chunk', choices:[{index:0, delta, finish_reason:reason}]}) + '\n\n');
        };
        if (body.stream) {
            res.writeHead(200, {'content-type':'text/event-stream', 'cache-control':'no-cache'});
            chunk({role:'assistant'});
        }
        const send = text => { if (body.stream) chunk({content:text}); };
        let output;
        if (user) {
            // A provider retry refers to the same real Alma user row. Repeated
            // intentional sends produce new row IDs, even if the text is identical.
            const existing = runs.get(user.id);
            if (existing) {
                output = (await existing.result).output;
                send(output);
            } else {
                const result = generate(fixture, marker, user.metadata?.optimisticId || user.id, send);
                runs.set(user.id, {marker, result});
                output = (await result).output;
            }
        } else {
            output = '此联调会话使用终端显示的测试文本。';
            send(output);
        }
        if (body.stream) {
            chunk({}, 'stop');
            if (!res.destroyed) res.end('data: [DONE]\n\n');
        } else {
            json(res, 200, {...header, object:'chat.completion', choices:[{index:0, message:{role:'assistant', content:output}, finish_reason:'stop'}]});
        }
    }

    async function result(marker) {
        if (![...runs.values()].some(run => run.marker === marker)) return false;
        const rows = await history();
        const index = rows.findIndex(r => r.message?.role === 'user' && textOf(r.message).trim() === marker);
        if (index < 0 || !runs.has(rows[index].id)) return false;
        const run = await runs.get(rows[index].id).result;
        const reply = rows[index + 1];
        if (reply?.message?.role !== 'assistant' || textOf(reply.message) !== run.output) return false;
        return run;
    }

    const status = await api('/api/mobile-relay/status');
    console.log(fixture.version + ', Alma reports phone online: ' + Boolean(status.phones?.connectedNow));
    const defaultModel = (await api('/api/settings')).chat?.defaultModel;
    const server = createServer((req, res) => handle(req, res).catch(() => {
        if (res.headersSent) res.destroy();
        else json(res, 502, {error:{message:'Vantry diagnostic request failed'}});
    }));
    try {
        server.listen(0, '127.0.0.1');
        await once(server, 'listening');
        const baseURL = 'http://127.0.0.1:' + server.address().port + '/v1';
        providerId = (await api('/api/providers','POST',{name:'Vantry 本机联调 ' + suffix, type:'custom',
            apiFormat:'openai-chat', baseURL, apiKey:password, models:[model], enabled:true})).id;
        assert(providerId);
        const selection = providerId + ':' + model;
        threadId = (await api('/api/threads','POST',{title, model:selection})).id;
        assert(threadId);
        const path = '/api/threads/' + encodeURIComponent(threadId);
        assert.equal((await api(path + '?tail=1')).model, selection);
        assert.equal((await api('/api/settings')).chat?.defaultModel, defaultModel);
        await api(path + '/send', 'POST', {text:desktop});
        const control = await waitFor(() => result(desktop), 60000, signal);
        console.log('Desktop control passed with ' + control.deltas + ' deltas and matching Core output.');
        if (desktopOnly) return;
        console.log('请在已配对的 iPhone 打开「' + title + '」，发送 ' + mobile);
        const phone = await waitFor(() => result(mobile), 20 * 60 * 1000, signal);
        console.log('Phone marker returned through Alma; Core run ' + phone.runId + ', ' + phone.deltas + ' deltas, matching saved reply.');
    } finally {
        try {
            if (providerId && threadId) {
                const path = '/api/threads/' + encodeURIComponent(threadId);
                const view = await api(path + '?tail=4');
                if (desktopOnly && view.messagesTotal <= 2 && (view.messages ?? []).every(row =>
                    row.message?.role === 'assistant' || textOf(row.message).trim() === desktop)) {
                    await api(path, 'DELETE');
                } else if (view.model === providerId + ':' + model) {
                    await api(path, 'PUT', {model:defaultModel || '', skipUpdatedAt:true});
                }
            }
        } finally {
            try { if (providerId) await api('/api/providers/' + encodeURIComponent(providerId), 'DELETE'); }
            finally {
                if (server.listening) {
                    server.closeAllConnections();
                    await new Promise(resolve => server.close(resolve));
                }
            }
        }
    }
}

const options = process.argv.slice(4);
if (process.argv.length < 4 || options.some(arg => arg !== '--desktop-only')) {
    throw new Error('Provide the service executable, official relay directory, and optionally --desktop-only');
}
const suffix = String(randomInt(100000, 1000000));
await withAlmaFixture(process.argv[2], process.argv[3], 'th_phone_' + suffix, fixture => probe(fixture, suffix, options.includes('--desktop-only')));
