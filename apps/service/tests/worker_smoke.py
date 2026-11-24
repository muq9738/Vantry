"""Exercise the shipped worker against an isolated OpenAI Chat Completions server."""
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import time

BINARY = sys.argv[1]
EXTENSION = '''export default function(pi) {
  pi.registerTool({
    name: "vantry_probe", label: "Probe", description: "Write the approved marker",
    parameters: {type:"object",properties:{},required:[]},
    async execute(_id, _args, _signal, _update, ctx) {
      const yes = await ctx.ui.confirm("Save marker?", "Allow this tool to write?", {timeout: TIMEOUT});
      if (yes) fs.writeFileSync(path.join(ctx.cwd, "marker"), "approved");
      return {content:[{type:"text",text:yes ? "marker saved" : "marker withheld"}]};
    }
  });
}
import * as fs from "node:fs";
import * as path from "node:path";
'''


def dump(path, value):
    path.parent.mkdir(exist_ok=True, parents=True)
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(value))
    tmp.replace(path)


def load(path):
    return json.loads(path.read_text())


class Model(BaseHTTPRequestHandler):
    def log_message(self, *_): pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.server.requests.append((self.path, self.headers.get('Authorization'), body))
        tools = [m['content'] for m in body['messages'] if m['role'] == 'tool']
        if tools:
            deltas = [({'role': 'assistant', 'content': 'recorded'}, None), ({}, 'stop')]
        else:
            call = {'index': 0, 'id': 'call_1', 'type': 'function',
                    'function': {'name': 'vantry_probe', 'arguments': '{}'}}
            deltas = [({'role': 'assistant', 'tool_calls': [call]}, None), ({}, 'tool_calls')]
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        for delta, finish in deltas:
            chunk = {'id':'completion','object':'chat.completion.chunk','created':1,'model':'fixture',
                     'choices':[{'index':0,'delta':delta,'finish_reason':finish}]}
            self.wfile.write(b'data: '+json.dumps(chunk).encode()+b'\n\n')
            self.wfile.flush()
        self.wfile.write(b'data: [DONE]\n\n')


@contextmanager
def running_model():
    with ThreadingHTTPServer(('127.0.0.1', 0), Model) as server:
        server.requests = []
        worker = threading.Thread(target=server.serve_forever)
        worker.start()
        try: yield server
        finally:
            server.shutdown()
            worker.join()


def run_case(root, server, decision):
    home, data, cwd = (root / name for name in ['home', 'data', 'work'])
    for d in (home, data, cwd): d.mkdir(parents=True)
    env = {'HOME': str(home), 'PATH': os.defpath, 'TMPDIR': str(root), 'PI_OFFLINE':'1',
           'NO_PROXY':'127.0.0.1,localhost', 'PI_SKIP_VERSION_CHECK':'1'}
    agent = home / '.pi/agent'
    if decision == 'allow':
        agent = home / 'explicit-pi'
        env['PI_CODING_AGENT_DIR'] = str(agent)
    assert not agent.exists()
    setup = {'providerId':'local','baseUrl':f'http://127.0.0.1:{server.server_port}/v1',
             'apiKey':'local-fixture','models':['fixture'],'setDefault':True}
    setup_cmd = [BINARY, 'pi-setup']
    subprocess.run(setup_cmd, input=json.dumps(setup), text=True, env=env, check=True, timeout=15)
    for filename in ['auth.json', 'settings.json', 'models.json']:
        path = agent / filename
        doc = load(path)
        if filename == 'auth.json': doc['other'] = {'type':'api_key','key':'unrelated'}
        if filename == 'models.json': doc['providers']['local']['models'][0]['name'] = 'Keep me'
        if filename == 'settings.json': doc.update({'extraSetting': True, 'retry': {'enabled': False}})
        dump(path, doc)
    subprocess.run(setup_cmd, input=json.dumps(setup), text=True, env=env, check=True, timeout=15)
    assert load(agent/'auth.json')['other']['key'] == 'unrelated'
    assert (agent/'auth.json').stat().st_mode & 0o777 == 0o600
    assert load(agent/'settings.json')['extraSetting']
    assert load(agent/'models.json')['providers']['local']['models'] == [{'id':'fixture','name':'Keep me'}]
    ext = agent/'extensions/probe.ts'
    ext.parent.mkdir()
    ext.write_text(EXTENSION.replace('TIMEOUT', '350' if decision == 'timeout' else '20000'))
    job = data / 'jobs/a1'
    req = {'dataDir':str(data), 'enableUserExtensions':True, 'request':{'ThreadID':'t1','AttemptID':'a1',
           'RunID':'r1','Message':'call the probe', 'ProviderID':'pi','Model':'local/fixture',
           'Workspace':{'kind':'directory','path':str(cwd)}, 'ApprovalPolicy':'ask'}}
    dump(job/'request.json', req)
    dump(job/'started.json', {'attemptId':'a1','state':'launching'})
    cmd = [BINARY, 'provider-worker', '--job-dir', str(job)]
    initial = len(server.requests)
    proc = subprocess.Popen(cmd, env=env, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            start_new_session=True, text=True)
    def deliver(control):
        subprocess.run([BINARY,'worker-control','--job-dir',str(job)],input=json.dumps(control),
                       text=True,env=env,check=True,timeout=10)
    responded = False
    def events():
        log = job/'events.jsonl'
        if not log.exists(): return []
        return [json.loads(line) for line in log.read_bytes().split(b'\n')[:-1]]
    try:
        deadline=time.monotonic()+40
        while proc.poll() is None:
            dialogs = [e['data'] for e in events() if e.get('type')=='extension_ui_request' and e['data'].get('method')=='confirm']
            if dialogs and not responded:
                responded = True
                if decision == 'crash':
                    os.killpg(proc.pid, signal.SIGKILL)
                    proc.wait(timeout=10)
                    with (job/'events.jsonl').open('ab') as journal: journal.write(b'{"sequence":')
                    recovery = subprocess.run(cmd, env=env, capture_output=True, timeout=10)
                    assert recovery.returncode != 0
                    assert (job/'events.partial').read_bytes() == b'{"sequence":'
                elif decision == 'cancel':
                    subprocess.run([BINARY, 'worker-stop','--job-dir',str(job)],env=env,check=True,timeout=10)
                elif decision in ('allow','deny','steer'):
                    if decision == 'steer':
                        deliver({'kind':'steer','id':'steer','message':'include the follow-up'})
                    response = {'kind':'response','id':'control:1','requestId':dialogs[0]['id'],'confirmed':decision=='allow'}
                    deliver(response)
                    deliver(response)
            if time.monotonic() > deadline: raise AssertionError(f'{decision} timed out: {[e.get("type") for e in events()]}')
            time.sleep(.015)
        logs = proc.communicate()[0]
        result = load(job/'result.json')
        expected_status = {'crash':'interrupted','cancel':'cancelled'}.get(decision,'completed')
        assert result['status']==expected_status, (decision, result, logs)
        assert responded, (decision, logs, events())
    finally:
        if proc.poll() is None: os.killpg(proc.pid, signal.SIGKILL)
        proc.wait()
        proc.stdout.close()
    log = events()
    assert [e['sequence'] for e in log] == list(range(1, result['lastSequence']+1))
    assert (cwd/'marker').exists() == (decision=='allow')
    assert (job/'prompt-intent.json').is_file()
    if decision == 'crash':
        assert len(server.requests) == initial + 1
    if decision not in ('cancel','crash'):
        assert any(e.get('type')=='tool_execution_end' for e in log)
        assert ''.join(e.get('delta','') for e in log)=='recorded'
        calls = server.requests[initial:]
        assert len(calls) >= 2 and all(path=='/v1/chat/completions' and key=='Bearer local-fixture' for path,key,_ in calls)
        if decision == 'steer':
            assert any('include the follow-up' in json.dumps(body['messages']) for _,_,body in calls)
            assert any(e.get('type')=='control_result' and e['data']=={'id':'steer','status':'delivered'} for e in log)
        expected='marker saved' if decision=='allow' else 'marker withheld'
        results = [m for m in calls[-1][2]['messages'] if m['role']=='tool']
        assert expected in json.dumps(results), results
        assert expected in (data/'sessions/t1.jsonl').read_text()
    if decision in ('allow','deny'):
        assert any(e.get('type')=='control_result' and e['data']['status']=='delivered' for e in log)
    if decision=='timeout': assert any(e.get('type')=='extension_ui_expired' for e in log)
    count = len(server.requests)
    subprocess.run(cmd, env=env, check=True, timeout=10)
    assert count==len(server.requests)
    if decision == 'allow':
        req['request'].update({'AttemptID':'a2','RunID':'r2','Message':'read previous result'})
        subprocess.run([BINARY,'smoke-worker'],input=json.dumps(req),text=True,env=env,check=True,timeout=20)
        last = server.requests[-1][2]['messages']
        assert 'marker saved' in json.dumps(last) and 'read previous result' in json.dumps(last)
        assert load(data/'jobs/a2/result.json')['status']=='completed'


with tempfile.TemporaryDirectory() as directory, running_model() as model:
    for decision in ['allow', 'deny', 'timeout', 'cancel', 'steer', 'crash']:
        run_case(Path(directory) / decision, model, decision)
