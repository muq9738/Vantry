// node alma_native_peer.mjs <host service> <alma-relay directory> [released client service]
import assert from 'node:assert/strict';
import {once} from 'node:events';
import {mkdir, readFile, writeFile} from 'node:fs/promises';
import {request} from 'node:http';
import {createServer} from 'node:net';
import {join, resolve} from 'node:path';
import {waitFor, withAlmaFixture} from './alma_interop_client.mjs';

const [host, relayDirectory, guest = host] = process.argv.slice(2);
const thread = 'native-alma-session';

async function api(socketPath, path, body) {
    const data = body === undefined ? undefined : JSON.stringify(body);
    const req = request({socketPath, path, method:data ? 'POST' : 'GET',
        headers:{'content-type':'application/json'}, signal:AbortSignal.timeout(20000)});
    const response = once(req, 'response');
    req.end(data);
    const [res] = await response;
    const chunks = [];
    for await (const chunk of res) chunks.push(chunk);
    return {status:res.statusCode, body:JSON.parse(Buffer.concat(chunks))};
}

async function workbench(socket, action, input, name) {
    const payload = input?.link ? {action, message:input.link, name} : {action, input, name};
    const res = await api(socket, '/v1/workbench', payload);
    assert.equal(res.status, 200, action);
    return JSON.parse(res.body.output);
}

async function exercise({data, root, start, relay, restart, signal}, offlinePrimary) {
    const studio = join(data, 'core.sock');
    const guestData = join(root, 'guest');
    const company = join(guestData, 'core.sock');
    await mkdir(guestData, {mode:0o700});
    start(resolve(guest), ['core','--data-dir',guestData,'--provider','controlled'], {HOME:root});
    await waitFor(() => api(company, '/v1/health').then(r => r.status === 200, () => false), 10000, signal);
    await workbench(studio, 'remote.access.approval', {enabled:true});
    const invite = await workbench(studio, 'remote.pair.create', {requireRelay:true});
    assert(invite.relayConnected);
    assert.equal(invite.relay, relay + '/');
    const bootstrap = JSON.parse(Buffer.from(invite.link.split('#')[1], 'base64url'));
    const endpoint = new URL(bootstrap.relays.at(-1));
    assert.equal(endpoint.origin, relay);
    assert.match(endpoint.pathname, /^\/c\/[A-Za-z0-9_-]{22}$/);
    assert.equal(bootstrap.relay, bootstrap.relays[0]);
    assert.deepEqual(bootstrap.relays, offlinePrimary ?
        [relay + '/offline' + endpoint.pathname, endpoint.href] : [endpoint.href]);
    assert.equal(bootstrap.secret, '');
    assert.equal((await fetch(endpoint + '/v1/status')).status, 403);
    bootstrap.direct = [];
    const link = 'vantry://connect#' + Buffer.from(JSON.stringify(bootstrap)).toString('base64url');
    const pending = await workbench(company, 'remote.pair.redeem', {link}, 'Company fixture');
    assert.equal(pending.status, 'pending');
    const [pendingRequest] = (await workbench(studio, 'remote.status')).requests;
    assert.equal(pendingRequest.verificationCode, pending.verificationCode);
    await workbench(studio, 'remote.requests.approve', {id:pendingRequest.id});
    const paired = await workbench(company, 'remote.pair.status', {handle:pending.handle});
    assert.equal(paired.status, 'paired');
    assert.equal((await api(company, '/v1/workbench', {action:'remote.pair.redeem',input:{link}})).status, 400);
    const saved = JSON.parse(await readFile(join(guestData,'remote-access.json')));
    assert.equal(saved.peers[0].pair.relay, bootstrap.relay);
    const proxy = join(root, 'peer.sock');
    start(resolve(guest), ['peer-proxy','--data-dir',guestData,'--peer',paired.id,'--mode','relay','--socket',proxy]);
    const remote = async path => (await api(proxy, path)).body;
    await waitFor(() => remote('/v1/status').then(r => !!r.version, () => false), 10000, signal);
    assert.deepEqual((await remote('/v1/access/identity')).relays, bootstrap.relays);
    assert((await remote('/v1/threads/summaries')).threads.some(t=>t.id === thread));
    const write = await api(proxy, '/v1/commands', {version:1, kind:'thread.create', commandId:'remote-create',
        threadId:'created-from-company', title:'Created over Alma'});
    assert(write.body.accepted);
    assert((await api(studio, '/v1/threads/summaries')).body.threads.some(t => t.id === 'created-from-company'));
    if (!offlinePrimary) await checkAutoRoutes(root, start, saved, paired.id, signal);
    await restart();
    await waitFor(() => remote('/v1/threads/summaries').then(r => r.threads?.some(t=>t.id === thread), () => false), 20000, signal);
    const [device] = (await workbench(studio, 'remote.status')).clients;
    assert.equal(device.name, 'Company fixture');
    assert((await workbench(studio, 'remote.peers.revoke', {id:device.id})).revoked);
    assert.notEqual((await api(proxy, '/v1/threads/summaries')).status, 200);
}

async function checkAutoRoutes(root, start, saved, peerId, signal) {
    const unreachable = createServer(socket => socket.destroy());
    unreachable.listen(0, '127.0.0.1');
    await once(unreachable, 'listening');
    try {
        for (const stale of [false, true]) {
            const dir = join(root, stale ? 'roaming' : 'on-lan');
            await mkdir(dir, {mode:0o700});
            const copy = structuredClone(saved);
            if (stale) copy.peers[0].pair.direct = ['127.0.0.1:' + unreachable.address().port];
            await writeFile(join(dir,'remote-access.json'), JSON.stringify(copy), {mode:0o600});
            const socket = join(dir,'p.sock');
            const proc = start(resolve(host), ['peer-proxy','--data-dir',dir,'--peer',peerId,'--mode','auto','--socket',socket],
                {HOME:root}, ['ignore','ignore','pipe']);
            let log = '';
            proc.stderr.on('data', data => { log += data.toString(); });
            const read = () => api(socket, '/v1/status').then(r => r.status === 200, () => false);
            await waitFor(read, 8000, signal);
            const first = stale ? 'relay' : 'direct';
            await waitFor(() => log.includes('Paired Core connected via ' + first), 2000, signal);
            // Keep using the same process and its warm HTTP pool while the LAN
            // address learned from the host becomes reachable. Both relays stay up.
            if (stale) await waitFor(async () => await read() && log.includes('Paired Core connected via direct'), 18000, signal);
            else assert(!log.includes('Paired Core connected via relay'));
        }
    } finally {
        await new Promise(resolve => unreachable.close(resolve));
    }
}

for (const offlinePrimary of [false, true]) {
    await withAlmaFixture(host, relayDirectory, thread, fixture => exercise(fixture, offlinePrimary), {
        cliHost:false,
        setup:({data, relay, registrationKey}) => {
            const settings = offlinePrimary ? {relay:relay + '/offline', registrationKey,
                fallbacks:[{relay, registrationKey}]} : {relay, registrationKey};
            return writeFile(join(data,'desktop-alma-relay.json'), JSON.stringify(settings), {mode:0o600});
        },
    });
}

await withAlmaFixture(host, relayDirectory, thread, async ({data,root,start,stopRelay,signal}) => {
    await stopRelay();
    const studio = join(data,'core.sock'), dir = join(root,'offline-guest');
    const invite = await workbench(studio, 'remote.pair.create');
    assert(!invite.relayConnected && invite.direct.length);
    await mkdir(dir, {mode:0o700});
    start(resolve(guest), ['core','--data-dir',dir,'--provider','controlled'], {HOME:root});
    const company = join(dir,'core.sock');
    await waitFor(() => api(company,'/v1/health').then(r => r.status === 200, () => false), 8000, signal);
    assert.equal((await workbench(company,'remote.pair.redeem',{link:invite.link},'Offline LAN')).status,'paired');
}, {cliHost:false, setup:({data,relay,registrationKey}) =>
    writeFile(join(data,'desktop-alma-relay.json'), JSON.stringify({relay,registrationKey}), {mode:0o600})});
console.log('Native pairing passed: LAN first, WAN fallback, warm relay back to LAN, offline LAN pairing, restart and revocation.');
