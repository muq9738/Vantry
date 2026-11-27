import assert from 'node:assert/strict';
import { createCipheriv, createDecipheriv, createECDH, createHash, hkdfSync, randomBytes, timingSafeEqual } from 'node:crypto';
import { once } from 'node:events';
import { execFile, spawn } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { promisify } from 'node:util';

const exec = promisify(execFile);

const context = 'alma-relay-e2e/v1';

function derive(secret, salt, info, size = 32) {
    return Buffer.from(hkdfSync('sha256', secret, salt, info, size));
}
export function routing(code) {
    return derive(code, context + '/routing', '', 16).toString('base64url');
}
function nonce(counter) {
    const iv = Buffer.alloc(12);
    iv.writeBigUInt64BE(counter, 4);
    return iv;
}

// An independent initiator using Node crypto. It speaks JSON inner frames, as negotiated.
export class Phone {
    constructor(url) {
        this.socket = new WebSocket(url);
        this.socket.binaryType = 'arraybuffer';
        this.incoming = 0n;
        this.outgoing = 0n;
        this.reader = new ReadableStream({
            start: controller => {
                this.socket.onmessage = e => controller.enqueue(e.data);
                this.socket.onerror = () => controller.error(new Error('Alma phone transport failed'));
                this.socket.onclose = () => { if (controller.desiredSize !== null) controller.close(); };
            },
        }).getReader();
    }
    async read() {
        const {done, value} = await deadline(this.reader.read());
        if (done) throw new Error('Alma phone disconnected');
        return value;
    }
    async pair(code) {
        await deadline(once(this.socket, 'open'));
        const ec = createECDH('prime256v1');
        const publicKey = ec.generateKeys();
        const n = randomBytes(16);
        this.socket.send(JSON.stringify({t:'hi', epk:publicKey.toString('base64url'), n:n.toString('base64url'), bin:1, sb:1, rw:1}));
        const hello = JSON.parse(await this.read());
        assert.equal(hello.t, 'hi');
        const peer = Buffer.from(hello.epk, 'base64url');
        const transcript = createHash('sha256').update(context).update(publicKey).update(peer)
            .update(n).update(Buffer.from(hello.n, 'base64url')).digest();
        const secret = ec.computeSecret(peer);
        const key = label => derive(secret, code, Buffer.concat([Buffer.from(label), transcript]));
        assert(timingSafeEqual(key('confirm-r|'), Buffer.from(hello.cr, 'base64url')), 'responder proof differs');
        this.socket.send(JSON.stringify({t:'ok', ci:key('confirm-i|').toString('base64url')}));
        this.rx = key('s2c|');
        this.tx = key('c2s|');
    }
    send(frame) {
        const iv = nonce(this.outgoing++);
        const cipher = createCipheriv('aes-256-gcm', this.tx, iv);
        const packet = Buffer.concat([iv, cipher.update(JSON.stringify(frame)), cipher.final(), cipher.getAuthTag()]);
        this.socket.send(packet);
        return packet;
    }
    async receive() {
        const packet = Buffer.from(await this.read());
        const iv = packet.subarray(0, 12);
        assert(iv.equals(nonce(this.incoming++)), 'unexpected response sequence');
        const dec = createDecipheriv('aes-256-gcm', this.rx, iv);
        dec.setAuthTag(packet.subarray(-16));
        return JSON.parse(Buffer.concat([dec.update(packet.subarray(12,-16)), dec.final()]));
    }
    async http(path, body, method = body ? 'POST' : 'GET') {
        this.send({t:'hreq', id:'h1', method, path,
            ...(body && {b:Buffer.from(JSON.stringify(body)).toString('base64url')})});
        const chunks = [];
        let status;
        while (true) {
            const f = await this.receive();
            if (f.id !== 'h1') continue;
            if (f.t === 'hres') status = f.status;
            else if (f.t === 'hchunk') chunks.push(Buffer.from(f.b, 'base64url'));
            else if (f.t === 'hend') return {status, body:JSON.parse(Buffer.concat(chunks))};
            else throw new Error('Unexpected HTTP frame');
        }
    }
}

export async function deadline(promise, ms = 7000) {
    let timer;
    try {
        return await Promise.race([promise, new Promise((_,reject) => {
            timer = setTimeout(() => reject(new Error('Timed out waiting for test peer')), ms);
        })]);
    } finally { clearTimeout(timer); }
}

export async function waitFor(check, timeoutMs = 8000, signal) {
    const until = Date.now() + timeoutMs;
    do {
        signal?.throwIfAborted();
        const result = await check();
        if (result) return result;
        await delay(100);
    } while (Date.now() < until);
    throw new Error('Timed out waiting for the test fixture');
}

async function stopChild(child) {
    if (!child.pid || child.exitCode !== null || child.signalCode) return;
    const exited = once(child, 'exit');
    child.kill('SIGTERM');
    try { await deadline(exited, 5000); }
    catch { child.kill('SIGKILL'); await deadline(exited, 5000); }
}

export async function withAlmaFixture(servicePath, relayDirectory, thread, exercise, {cliHost = true, setup, threadArgs = []} = {}) {
    const service = resolve(servicePath);
    const relayEntry = resolve(relayDirectory, 'index.mjs');
    const version = (await exec(service, ['--version'])).stdout.trim();
    const root = await mkdtemp(join(tmpdir(), 'alma-'));
    const data = join(root, 'core');
    const children = [];
    const phones = new Set();
    const cancel = new AbortController();
    const stop = () => cancel.abort(new Error('Test interrupted'));
    process.once('SIGTERM', stop);
    process.once('SIGINT', stop);
    const start = (binary, args, env = {}, stdio = 'ignore') => {
        const child = spawn(binary, args, {env: {...process.env, ...env}, stdio});
        children.push(child);
        return child;
    };
    const ctl = async (...args) => JSON.parse((await exec(service, ['ctl', ...args, '--data-dir', data], {timeout:5000})).stdout);
    try {
        await mkdir(data, {mode:0o700});
        const gate = randomBytes(24).toString('base64url');
        await writeFile(join(data, 'alma-registration-key'), gate, {mode:0o600});
        const listener = createServer();
        listener.listen(0, '127.0.0.1');
        await once(listener, 'listening');
        const port = listener.address().port;
        await new Promise(resolve => listener.close(resolve));
        const relay = 'http://127.0.0.1:' + port;
        const fixtureEnv = await setup?.({root, data, relay, registrationKey:gate}) || {};
        const agentCount = async () => (await (await fetch(relay + '/healthz', {signal:AbortSignal.timeout(3000)})).json()).agents;
        const startRelay = () => start(process.execPath, [relayEntry], {HOST:'127.0.0.1', PORT:String(port), RELAY_REGISTRATION_KEY:gate, RELAY_TOKEN_SECRET:''});
        let relayProcess = startRelay();
        const startCore = () => start(service, ['core', '--data-dir', data, '--workspace', root, '--provider', 'controlled',
            '--controlled-delay', '80ms', ...(cliHost ? ['--alma-thread', thread, '--alma-relay', relay] : [])], fixtureEnv);
        let core = startCore();
        const registered = () => waitFor(async () => {
            assert(core.exitCode === null && relayProcess.exitCode === null, 'Fixture process exited early');
            try {
                const agents = await agentCount();
                if (cliHost) return agents === 1;
                await ctl('status');
                return true;
            } catch { return false; }
        }, 10000, cancel.signal);
        await registered();
        await ctl('create', '--thread', thread, '--title', 'Alma interop', ...threadArgs);
        const code = cliHost ? JSON.parse(await readFile(join(data, 'alma-pairing.json'))).code : '';
        const url = cliHost ? relay + '/c/' + routing(code) : '';
        const connectTo = async (base, key) => {
            const phone = new Phone(base.replace(/^http/, 'ws') + '/__alma_e2e__');
            phones.add(phone);
            phone.socket.addEventListener('close', () => phones.delete(phone), {once:true});
            await phone.pair(key);
            return phone;
        };
        const restart = async () => {
            await stopChild(core);
            await waitFor(async () => await agentCount() === 0, 8000, cancel.signal);
            core = startCore();
            await registered();
        };
        return await exercise({ctl, connect:() => connectTo(url,code), connectTo, restart, thread, url,
            relay, registrationKey:gate, version, signal:cancel.signal, root, data, start,
            stopRelay:() => stopChild(relayProcess), resumeRelay:() => { relayProcess = startRelay(); }});
    } finally {
        try {
            for (const p of phones) p.socket.close();
            const stopped = await Promise.allSettled(children.map(stopChild));
            const failures = stopped.filter(r => r.status === 'rejected');
            if (failures.length) throw new AggregateError(failures.map(r => r.reason), 'Fixture process still running');
            await rm(root, {recursive:true, force:true});
        } finally {
            process.off('SIGTERM', stop);
            process.off('SIGINT', stop);
        }
    }
}
