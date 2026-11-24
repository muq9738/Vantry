"""Black-box service/CLI contract, including an abrupt Core restart mid-stream."""
import base64
import contextlib
import http.client
import json
import os
from pathlib import Path
import signal
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import socket
import subprocess
import sys
import tempfile
import time
import zipfile

BIN = sys.argv[1]


def processes():
    lines = subprocess.check_output(['ps', '-Ao', 'pid=,command='], text=True).splitlines()
    return {int(pid): cmd for line in lines if line.strip()
            for pid, cmd in [line.strip().split(None, 1)]
            if cmd.startswith(BIN + ' core ') or cmd.startswith(BIN + ' provider-worker ')}


def until(check, seconds=12):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        result = check()
        if result:
            return result
        time.sleep(.025)
    raise AssertionError(f'timeout waiting for {check}')


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=8)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def exercise(root):
    data, work, home = [root / name for name in ['data', 'work', 'home']]
    work.mkdir(); home.mkdir()
    env = dict(HOME=str(home), VANTRY_DATA_DIR=str(data), TMPDIR=str(root), PATH=os.defpath,
               PI_CODING_AGENT_DIR=str(home/'pi'), PI_OFFLINE='1', PI_SKIP_VERSION_CHECK='1')
    def execute(args, **kw):
        return subprocess.run(args, text=True, env=env, capture_output=True, timeout=15, **kw)

    def ctl(*args):
        result = execute([BIN, 'ctl', '-data-dir', str(data), *args])
        assert result.returncode == 0, (args, result.stderr)
        return json.loads(result.stdout)

    def api(path, body=None, success=True):
        with contextlib.closing(UnixHTTP(data/'core.sock')) as conn:
            conn.request('GET' if body is None else 'POST', '/v1/'+path,
                         body=None if body is None else json.dumps(body),
                         headers={'Content-Type': 'application/json'})
            res = conn.getresponse(); value = json.load(res)
            assert (res.status == 200) == success, (path, res.status, value)
            return value

    def wb(action, **kwargs): return api('workbench', dict(action=action, **kwargs))
    def broker(action, **kwargs): return api('orchestration/'+action, kwargs)

    children = []
    def start(delay):
        proc = subprocess.Popen([BIN, 'core', '-data-dir', data, '--provider', 'controlled',
                                 '--listen-remote','127.0.0.1:0',
                                 '-controlled-delay', delay, '--workspace', work], env=env,
                                stdout=subprocess.DEVNULL)
        children.append(proc)
        def ready():
            assert proc.poll() is None, 'core stopped during startup'
            try: return api('health')['state'] == 'ready'
            except (FileNotFoundError, ConnectionRefusedError): return False
        until(ready)
        return proc

    before = processes()
    def ours(): return {p:c for p,c in processes().items() if p not in before and str(root) in c}
    try:
        execute(['git', 'init', '-q', str(work)], check=True)
        (work/'note.txt').write_text('first')
        execute(['git','-C',str(work),'add','note.txt'], check=True)
        execute(['git','-C',str(work),'-c','user.name=fixture','-c','user.email=test@localhost',
                 'commit','-qm','initial'], check=True)
        (work/'note.txt').write_text('changed')
        core = start('10ms')
        other=root/'other'; peer=subprocess.Popen([BIN,'core','--data-dir',other],env=env)
        children.append(peer);until(lambda:(other/'core.sock').exists())
        def peer_ctl(*args):
            result=execute([BIN,'ctl','--data-dir',str(other),*args],check=True)
            return json.loads(result.stdout)
        invitation=json.loads(wb('remote.pair.create')['output'])
        peer_id=json.loads(peer_ctl('workbench','remote.pair.redeem','--message',invitation['link'],'--name','fixture')['output'])['id']
        assert json.loads(peer_ctl('workbench','remote.peers.list')['output'])['peers'][0]['id']==peer_id
        assert json.loads(peer_ctl('workbench','remote.peers.revoke','--name',peer_id)['output'])['revoked']
        peer_ctl('shutdown-if-idle');assert peer.wait(timeout=5)==0
        with socket.socket() as bound:
            bound.bind(('127.0.0.1',0)); relay_port=bound.getsockname()[1]
        config=root/'relay.json'; key=os.urandom(24).hex()
        config.write_text(json.dumps({'desktop':{'registrationKey':key}}))
        relay=subprocess.Popen([BIN,'relay','serve','-config',config,'--listen',f'127.0.0.1:{relay_port}'],env=env)
        children.append(relay)
        def relay_ok():
            with contextlib.closing(http.client.HTTPConnection('127.0.0.1',relay_port,timeout=1)) as conn:
                try:conn.request('GET','/healthz');return conn.getresponse().status==200
                except ConnectionRefusedError:return False
        until(relay_ok)
        room=json.loads(wb('rd.session.start',input={'registrationKey':key,'relay':f'http://127.0.0.1:{relay_port}'})['output'])
        assert json.loads(wb('rd.session.status')['output'])['sessionId']==room['sessionId']
        assert json.loads(wb('rd.session.stop')['output'])['active']==False
        relay.terminate();assert relay.wait(timeout=5)==0
        assert ctl('health')['state'] == 'ready'
        assert ctl('status')['sequence'] >= 0
        assert 'orchestration.v1' in api('capabilities')['capabilities']
        receipt = ctl('create','-command-id','fixture','--title','fixture')
        thread = receipt['threadId']
        assert ctl('create','--command-id','fixture','--title','fixture') == receipt
        watch_file = (root/'watch.ndjson').open('w+')
        watch = subprocess.Popen([BIN,'ctl','watch','--after',str(receipt['sequence'])],env=env,stdout=watch_file)
        children.append(watch)
        run = ctl('send','--thread',thread,'-message','hello')['runId']
        result = ctl('thread-wait','--thread',thread,'--run',run)
        assert result['status'] == 'completed' and 'hello' in result['summary'], result
        assert ctl('thread-read','--thread',thread,'--view','activity')['items']
        assert ctl('threads')['threads'][0]['threadId'] == thread
        assert ctl('snapshot')['runs'][0]['status']=='completed'
        ctl('snooze','--thread',thread,'--duration','1h30m')
        assert ctl('threads')['threads'][0]['snoozed']
        ctl('unsnooze','--thread',thread)
        assert not ctl('threads')['threads'][0]['snoozed']
        assert ctl('workspace','--thread',thread)['workspace']==str(work)
        fork=ctl('fork','--thread',thread,'--run',run,'--title','fork')['threadId']
        fork_run=ctl('send','--thread',fork,'--message','result')['runId']
        assert ctl('thread-wait','--thread',fork,'--run',fork_run)['status']=='completed'
        ctl('merge-back','--thread',fork,'--target',thread)
        assert ctl('thread-wait','--thread',thread)['status']=='completed'
        ctl('rewind','--thread',fork,'--run',fork_run);ctl('delete','--thread',fork)
        assert broker('threads/search',query='hello')['hits']
        assert broker('capabilities')['inheritedProviderId']=='controlled'
        launched=broker('threads/launch',clientRequestId='launch',callerThreadId=thread,title='another')
        assert broker('threads/launch',clientRequestId='launch',callerThreadId=thread,title='another')==launched
        assert len(broker('threads/create-batch',clientRequestId='batch',threads=[{'title':'a'},{'title':'b'}])['threads'])==2
        assert broker('tasks/status',callerThreadId=thread)['tasks']==[]
        assert broker('task-status',callerThreadId=thread)['tasks']==[]
        parent=ctl('send','--thread',thread,'--message','coordinate '*50)['runId']
        delegated=broker('delegate',parentRunId=parent,callerThreadId=thread,task='work '*150,clientRequestId='delegate')
        task=delegated['taskId']; assert broker('tasks/status',taskId=task)['tasks'][0]['parentRunId']==parent
        assert broker('tasks/cancel',callerThreadId=thread,taskId=task)['status']=='cancelled'
        assert ctl('thread-wait','--thread',thread,'--run',parent)['status']=='completed'
        api('orchestration/tasks/cancel',{'taskId':'missing'},False)
        assert broker('threads/send',targetThreadId=thread,message='second',clientRequestId='second')['sequence']>0
        assert ctl('thread-wait','--thread',thread)['status']=='completed'
        for action in ['models','mcp.list','sandbox.get','projects.list','environments.list',
                       'plugins.list','usage.summary','environment.info','remote.status','rd.status','update.status']:
            response = wb(action)
            assert isinstance(response,dict), action
        usage=json.loads(wb('usage.summary')['output'])
        assert usage['completedRuns']>=1 and usage['models'] and usage['threads']>=1
        assert isinstance(api('models'),list)
        # Probe a real MCP stdio transport in the isolated workspace.
        mcp_source = work/'fixture.py'
        mcp_source.write_text('''import json,sys
for line in sys.stdin:
    req=json.loads(line)
    if 'id' not in req: continue
    result={'protocolVersion':'2025-03-26','capabilities':{}} if req['method']=='initialize' else {'tools':[{'name':'echo'}]}
    print(json.dumps({'jsonrpc':'2.0','id':req['id'],'result':result}),flush=True)
''')
        wb('mcp.upsert',name='fixture',input={'scope':'project','config':{'command':sys.executable,'args':[str(mcp_source)]}})
        assert wb('mcp.probe')['mcp']['servers'][0].get('tools',[])==[]
        wb('mcp.trust',input={'trusted':True})
        assert wb('mcp.probe')['mcp']['servers'][0]['tools']==['echo']
        wb('mcp.remove',name='fixture',input={'scope':'project'}); mcp_source.unlink()
        project=json.loads(wb('projects.add',workspace=str(work))['output'])['projects'][0]
        assert json.loads(wb('projects.remove',name=project['id'])['output'])['projects']==[]
        api('workbench', {'action':'ssh.hosts'}, False)
        api('workbench', {'action':'environments.upsert','input':{'id':'old','name':'legacy','kind':'ssh','host':'fixture'}}, False)
        entries=json.loads(wb('environments.upsert',input={'id':'lab','name':'fixture','kind':'remote','host':'fixture'})['output'])['environments']
        assert any(e['id']=='lab' for e in entries)
        assert not any(e['id']=='lab' for e in json.loads(wb('environments.remove',name='lab')['output'])['environments'])
        archive=root/'panel.zip'
        with zipfile.ZipFile(archive,'w') as zip_file:
            zip_file.writestr('plugin.json',json.dumps({'id':'panel','name':'Panel','version':'1.0','runtime':'native',
                'navigation':[{'title':'Home','page':'home'}], 'pages':{'home':{'vantryPanel':{'version':1,'widgets':[{'kind':'text','text':'fixture'}]}}}}))
        assert wb('plugins.install',name=str(archive))['plugins'][0]['id']=='panel'
        assert json.loads(wb('plugins.panel',pluginId='panel',name='home')['output'])['vantryPanel']['version']==1
        assert wb('plugins.disable',pluginId='panel')['plugins'][0]['disabled']
        api('workbench',{'action':'plugins.run','pluginId':'panel'},False)
        wb('plugins.enable',pluginId='panel'); assert wb('plugins.run',pluginId='panel')['output']
        assert not wb('plugins.uninstall',pluginId='panel').get('plugins')
        assert ctl('diff','--thread',thread)['isGit']
        assert 'changed' in api('workspace?thread='+thread)['diff']
        assert json.loads(wb('sandbox.set',input={'mode':'off'})['output'])['mode']=='off'
        assert any(f['path']=='note.txt' for f in wb('git.status')['git']['files'])
        assert 'changed' in wb('git.diff')['output']
        wb('git.stage',paths=['note.txt'])
        assert 'changed' in wb('git.diff',input={'staged':True})['output']
        wb('git.unstage',paths=['note.txt'])
        assert wb('git.branches')['git']['branches']
        assert json.loads(wb('files.list')['output'])['entries']
        assert wb('files.read',paths=['note.txt'])['output']=='changed'
        picture=base64.b64decode('R0lGODlhAQABAAAAACwAAAAAAQABAAA=')
        (work/'tiny.gif').write_bytes(picture)
        assert base64.b64decode(wb('reply.image',threadId=thread,name='tiny.gif')['data'])==picture
        (work/'tiny.gif').unlink()
        api('workbench',{'action':'reply.image','threadId':thread,'name':'note.txt'},False)
        api('workbench',{'action':'files.read','paths':['../data/state.sqlite']},False)
        assert ctl('workbench','files.read','--path','note.txt')['output']=='changed'
        item=wb('attachment.import',name='hello.txt',data=base64.b64encode(b'hello').decode())['attachment']
        assert base64.b64decode(wb('attachment.read',input=item)['data'])==b'hello'
        ctl('shutdown-if-idle'); assert core.wait(timeout=6)==0
        watch.wait(timeout=5); watch_file.seek(0)
        events=[json.loads(line) for line in watch_file]; watch_file.close()
        assert any(e['type']=='run.completed' and e['runId']==run for e in events)
        assert all(a['sequence']<b['sequence'] for a,b in zip(events,events[1:]))

        core=start('80ms')
        run=ctl('send','--thread',thread,'--message','recovery '*45)['runId']
        def active_job():
            for path in (data/'jobs').glob('*/worker.json'):
                req=json.loads((path.parent/'request.json').read_text())['request']
                if req['RunID']==run and (path.parent/'observer-cursor.json').exists():return path.parent
        job=until(active_job)
        cursor=json.loads((job/'observer-cursor.json').read_text()); assert cursor['sequence']>0 and cursor['offset']>0
        marker=json.loads((job/'worker.json').read_text()); pid=marker['pid']
        core.kill(); core.wait(timeout=5); os.kill(pid,0)
        core=start('10ms'); result=ctl('thread-wait','--thread',thread,'--run',run)
        assert result['status']=='completed',result
        assert json.loads((job/'worker.json').read_text())['pid']==pid
        events=[json.loads(line) for line in (job/'events.jsonl').read_text().splitlines()]
        assert [e['sequence'] for e in events]==list(range(1,len(events)+1))
        record=next(r for r in ctl('snapshot')['runs'] if r['id']==run)
        assert record['output']==''.join(e.get('delta','') for e in events if e['type']=='text_delta')
        assert json.loads((job/'observer-cursor.json').read_text())['offset']==(job/'events.jsonl').stat().st_size
        run=ctl('send','--thread',thread,'--message','stop '*500)['runId']
        job=until(active_job); ctl('stop','--thread',thread,'--run',run)
        until((job/'result.json').exists)
        assert json.loads((job/'result.json').read_text())['status']=='cancelled'
        assert ctl('thread-wait','--thread',thread,'--run',run)['status']=='cancelled'
        # Hold the real embedded Pi worker between two SSE frames while replacing Core.
        release = threading.Event()
        requests = []
        class Model(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_POST(self):
                requests.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
                self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.end_headers()
                for delta,finish in [({'role':'assistant','content':'first'},None),({'content':' second'},None),({},'stop')]:
                    chunk={'id':'stream','object':'chat.completion.chunk','created':1,'model':'fixture',
                           'choices':[{'index':0,'delta':delta,'finish_reason':finish}]}
                    self.wfile.write(b'data: '+json.dumps(chunk).encode()+b'\n\n'); self.wfile.flush()
                    if delta.get('content')=='first': release.wait(timeout=12)
                self.wfile.write(b'data: [DONE]\n\n')
        with ThreadingHTTPServer(('127.0.0.1',0),Model) as server:
            serving=threading.Thread(target=server.serve_forever); serving.start()
            try:
                wb('pi.setup.save',input={'providerId':'isolated','models':['fixture'],'apiKey':'fixture',
                   'baseUrl':f'http://127.0.0.1:{server.server_port}/v1','setDefault':True})
                pi_thread=ctl('create','--provider','pi','--model','isolated/fixture')['threadId']
                pi_run=ctl('send','--thread',pi_thread,'--message','stream once')['runId']
                def first_output():
                    run=next(r for r in ctl('snapshot')['runs'] if r['id']==pi_run)
                    assert run['status']!='failed',run.get('error')
                    return run['output']=='first'
                until(first_output);core.kill();core.wait(timeout=5);core=start('10ms');release.set()
                assert ctl('thread-wait','--thread',pi_thread,'--run',pi_run)['summary']=='first second'
                assert len(requests)==1
            finally:
                release.set();server.shutdown();serving.join()
        ctl('shutdown-if-idle');assert core.wait(timeout=5)==0
        ensure=execute([BIN,'ensure-core','--provider','controlled','--data-dir',str(data)],check=True)
        assert json.loads(ensure.stdout)['state']=='ready'
        ctl('shutdown-if-idle');until(lambda:not ours())
        assert not (data/'core.sock').exists()
    finally:
        for child in reversed(children):
            if child.poll() is None: child.terminate()
            try: child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill(); child.wait(timeout=5)
        for pid in ours():
            try:os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:pass
        until(lambda:not ours())


with tempfile.TemporaryDirectory(prefix='vt4-',dir='/tmp') as temporary:
    exercise(Path(temporary).resolve())
assert not Path(temporary).exists()
