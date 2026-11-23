#!/usr/bin/env python3
"""Verify a private RTC ticket in a real MyGo viewer window.

The remote host must be explicitly prepared by the operator. This test sends
no mouse/keyboard events to the remote screen and never prints its ticket.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

from smoke_desktop import Automation, get_json, terminate, wait_until


def smoke(binary, helper, plugin, ticket_file, artifacts, duration):
    if ticket_file.stat().st_mode & 0o777 != 0o600:
        raise RuntimeError('viewer ticket file must be 0600')
    ticket=ticket_file.read_text().strip()
    artifacts.mkdir(parents=True,exist_ok=True)
    artifacts.chmod(0o700)
    session=Path(tempfile.mkdtemp(prefix='vantry-rtc-ui-',dir='/tmp'))
    data=session/'data';data.mkdir(mode=0o700)
    installed=data/'plugins/vantry-remote-desktop'
    installed.mkdir(parents=True)
    for name in ['plugin.json','index.ts']:
        shutil.copy2(plugin/name,installed/name)
    sock=data/'core.sock';automation=session/'ui.sock'
    report={'checks':[],'remote_input_sent':False,'screen_pixels_saved':False}
    owned=[];logs=[];client=None;failure=None
    try:
        environment={k:v for k,v in os.environ.items() if k!='MYGO_AUTOMATION'}
        environment['VANTRY_REMOTE_HELPER']=str(helper)
        for name in ['core','desktop']:
            log=(artifacts/(name+'.log')).open('w');logs.append(log)
            command=[str(binary),name,'--data-dir',str(data),'--provider','controlled']
            if name=='desktop':command+=['--attach-only'];env=dict(environment,MYGO_AUTOMATION=str(automation))
            else:env=environment
            process=subprocess.Popen(command,env=env,stdout=log,stderr=subprocess.STDOUT);owned.append(process)
            if name=='core':wait_until('independent Core ready',lambda:get_json(sock,'/v1/health')['state']=='ready')
        wait_until('native viewer automation',automation.exists)
        client=Automation(automation,process_id=owned[-1].pid)
        wait_until('viewer window',lambda:client.call('windows')['windows'])
        client.activate()
        report['native_window_focused']=any(w.get('focused') for w in client.call('windows')['windows'])
        def tree():
            client.call('capture')
            return client.tree()
        def control(label,role='button'):
            return next((n for n in tree() if n.get('label')==label and n.get('role')==role),None)
        wait_until('contributed RTC plugin page',lambda:control('远程桌面'),timeout=15)
        client.press('远程桌面')
        field=wait_until('private ticket input',lambda:control('远程桌面目标','textField'))
        client.call('act',{'id':field['id'],'action':'setValue','text':ticket})
        client.call('capture');client.press('立即远程连接')
        def receiving():
            nodes=tree()
            viewport=next((n for n in nodes if n.get('label')=='远程桌面画面'),None)
            waiting=any(n.get('label')=='等待第一帧画面…' for n in nodes)
            return viewport if viewport and not waiting else None
        viewport=wait_until('native viewer displays its first RTC frame',receiving,timeout=25)
        report['checks'].extend(['production viewer factory is injected','RTC first frame reaches native viewport'])
        end=time.monotonic()+duration
        hud=[]
        while time.monotonic()<end:
            nodes=tree()
            for node in nodes:
                label=node.get('label','')
                if 'WebRTC / H.264' in label and 'FPS' in label:
                    if ticket in label or 'token=' in label:raise RuntimeError('private ticket reached a visible status label')
                    hud=[label]
            time.sleep(.05)
        if not hud:raise RuntimeError('RTC codec/transport/frame-rate HUD never appeared')
        report['hud']=hud
        report['checks'].append('measured RTC transport and frame rate reach the native HUD')
        client.press('断开远程桌面')
        wait_until('viewer disconnect returns to ticket input',lambda:control('远程桌面目标','textField'))
        report['checks'].append('explicit disconnect releases its RTC viewer')
        report['status']='passed'
    except Exception as error:
        failure=error;report.update(status='failed',error=str(error))
        if client:
            try:
                report['visible_errors']=[node.get('label','').replace(ticket,'<private ticket>') for node in client.tree() if any(word in node.get('label','') for word in ['失败','未链接','rtc:','not ready','not open','断开']) and 'token=' not in node.get('label','')]
            except Exception: pass
    finally:
        if client:client.close()
        cleanup=[]
        for process in reversed(owned):
            try:terminate(process)
            except Exception as error:cleanup.append(str(error))
        for log in logs:log.close()
        report['processes']=[{'pid':p.pid,'returncode':p.poll()} for p in owned]
        if all(p.poll() is not None for p in owned):shutil.rmtree(session);report['temporary_directory_removed']=True
        else:report['retained_directory']=str(session)
        if cleanup:report.update(status='failed',cleanup_errors=cleanup)
        path=artifacts/'result.json';path.write_text(json.dumps(report,ensure_ascii=False,indent=2));path.chmod(0o600)
    if failure or report.get('status')!='passed':raise RuntimeError(json.dumps(report,ensure_ascii=False)) from failure
    return report


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ['binary','helper','plugin','ticket-file','artifacts']:parser.add_argument('--'+name,type=Path,required=True)
    parser.add_argument('--duration',type=float,default=6)
    args=parser.parse_args()
    print(json.dumps(smoke(args.binary.resolve(),args.helper.resolve(),args.plugin.resolve(),args.ticket_file.resolve(),args.artifacts.resolve(),args.duration),ensure_ascii=False,indent=2))
