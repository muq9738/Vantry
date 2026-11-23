#!/usr/bin/env python3
"""Exercise Core -> detached worker -> controlled Pi RPC, including Core SIGKILL.

Uses only a local protocol fixture; it never sends a model request.
Build with: go build -o bin/vantry ./cmd/vantry
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

from smoke_desktop import get_json, terminate, wait_until

FIXTURE = r'''
import json, os, sys, threading, time
from pathlib import Path
output_lock = threading.Lock()
finish = Path(os.environ["VANTRY_FIXTURE_FINISH"])
log = Path(os.environ["VANTRY_FIXTURE_INPUTS"])
cwd_file = Path(os.environ["VANTRY_FIXTURE_CWD"])
started = threading.Event()
def emit(record):
    with output_lock:
        print(json.dumps(record, ensure_ascii=False), flush=True)
def complete():
    started.wait()
    while not finish.exists(): time.sleep(0.02)
    emit({"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"RPC_FINISHED\n"}})
    emit({"type":"agent_end"})
    emit({"type":"agent_settled","aborted":False})
threading.Thread(target=complete,daemon=True).start()
for line in sys.stdin:
    command = json.loads(line)
    with log.open("a") as output: output.write(json.dumps(command,ensure_ascii=False)+"\n")
    kind = command["type"]
    if kind == "get_state":
        emit({"id":command["id"],"type":"response","command":"get_state","success":True,"data":{"isStreaming":False,"messageCount":0,"model":{"provider":"magpie","id":"fixture-model"}}})
    elif kind == "prompt":
        cwd_file.write_text(os.getcwd())
        emit({"id":command["id"],"type":"response","command":"prompt","success":True,"data":{"disposition":"started"}})
        emit({"type":"agent_start"})
        emit({"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"RPC_READY\n"}})
        started.set()
    elif kind == "steer":
        emit({"id":command["id"],"type":"response","command":"steer","success":True,"data":{"disposition":"queued"}})
        emit({"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"STEER_ACCEPTED: "+command["message"]+"\n"}})
    else:
        emit({"id":command.get("id"),"type":"response","command":kind,"success":False,"error":"unexpected fixture command"})
'''


def smoke(binary, artifacts, without_project=False):
    artifacts.mkdir(parents=True, exist_ok=True)
    result = {"checks": [], "llmPromptsSent": 0}
    core = None
    try:
        with tempfile.TemporaryDirectory(prefix="vantry-steering-", dir="/tmp") as root:
            root = Path(root)
            data, project = root / "data", root / "project"
            project.mkdir()
            finish, inputs = root / "finish", root / "inputs.jsonl"
            cwd_file = root / "execution-cwd"
            model_settings = root / "magpie-settings.json"
            model_settings.write_text(json.dumps({"pickedModels":{"pi":["fixture-model"]}}))
            fixture = root / "pi-fixture"
            fixture.write_text("#!" + sys.executable + "\n" + FIXTURE)
            fixture.chmod(0o700)
            env = dict(os.environ, VANTRY_FIXTURE_FINISH=str(finish), VANTRY_FIXTURE_INPUTS=str(inputs), VANTRY_FIXTURE_CWD=str(cwd_file))
            env.pop("MYGO_AUTOMATION", None)
            socket = data / "core.sock"
            arguments = [str(binary), "core", "--data-dir", str(data), "--provider", "pi", "--model", "magpie/fixture-model", "--pi-executable", str(fixture), "--magpie-settings", str(model_settings)]
            if not without_project:
                arguments += ["--workspace", str(project)]
            with (artifacts / "core.log").open("w") as core_log:
                def start():
                    process = subprocess.Popen(arguments, env=env, stdout=core_log, stderr=subprocess.STDOUT)
                    wait_until("Core ready", lambda: get_json(socket, "/v1/health")["state"] == "ready")
                    return process

                def ctl(*args, check=True):
                    response = subprocess.run([str(binary), "ctl", "--data-dir", str(data), *args], env=env, text=True, capture_output=True, timeout=15)
                    if check:
                        assert response.returncode == 0, response.stderr
                        return json.loads(response.stdout)
                    return response

                def snapshot():
                    return get_json(socket, "/v1/snapshot")

                try:
                    core = start()
                    thread = ctl("create", "--command-id", "create-once", "--title", "Steering RPC 验收")["threadId"]
                    run = ctl("send", "--command-id", "prompt-once", "--thread", thread, "--message", "哈喽" if without_project else "保持运行，等待明确结束信号")["runId"]
                    before = wait_until("running Pi RPC", lambda: next((r for r in snapshot()["runs"] if r["id"] == run and r["status"] == "running" and "RPC_READY" in r["output"]), None))
                    actual_cwd = Path(cwd_file.read_text()).resolve()
                    expected_cwd = (data / "conversations" / thread if without_project else project).resolve()
                    assert actual_cwd == expected_cwd, f"wrong execution directory: {actual_cwd} != {expected_cwd}"
                    if without_project:
                        assert next(t for t in snapshot()["threads"] if t["id"] == thread).get("workspace") is None
                    result["withoutProject"] = without_project
                    result["correctExecutionDirectory"] = True
                    result["checks"].append("projectless greeting uses a private conversation directory" if without_project else "explicit project remains the execution directory")
                    result["checks"].append("one initial prompt reaches detached Pi RPC")
                    args = ("steer", "--command-id", "steer-once", "--thread", thread, "--run", run, "--message", "先检查取消操作")
                    receipt = ctl(*args)
                    wait_until("first steering accepted", lambda: any(m["id"] == receipt["messageId"] and m["status"] == "accepted" for m in snapshot().get("steering", [])))
                    assert ctl(*args) == receipt
                    result["checks"].append("steering accepted by same attempt; duplicate command returns original receipt")
                    core.kill()
                    core.wait(timeout=5)
                    core = None
                    core = start()
                    recovered = snapshot()
                    assert next(r for r in recovered["runs"] if r["id"] == run)["attemptId"] == before["attemptId"]
                    assert ctl(*args) == receipt
                    result["checks"].append("Core SIGKILL and restart preserve attempt and steering identity")
                    second = ctl("steer", "--command-id", "steer-second", "--thread", thread, "--run", run, "--message", "再核对附件")
                    wait_until("second steering accepted after recovery", lambda: any(m["id"] == second["messageId"] and m["status"] == "accepted" for m in snapshot().get("steering", [])))
                    finish.touch()
                    completed = wait_until("same run completes", lambda: next((r for r in snapshot()["runs"] if r["id"] == run and r["status"] == "completed"), None))
                    assert completed["attemptId"] == before["attemptId"]
                    assert "先检查取消操作" in completed["output"] and "再核对附件" in completed["output"] and "RPC_FINISHED" in completed["output"]
                    records = [json.loads(line) for line in inputs.read_text().splitlines()]
                    assert sum(r["type"] == "prompt" for r in records) == 1, records
                    assert sum(r["type"] == "steer" for r in records) == 2, records
                    rejected = ctl("steer", "--command-id", "steer-too-late", "--thread", thread, "--run", run, "--message", "终态不应执行", check=False)
                    assert rejected.returncode != 0 and "conflict" in rejected.stderr, rejected.stderr
                    result["checks"].extend(["second steering reaches original Pi subprocess after Core recovery", "terminal run rejects steering", "exactly one prompt and two distinct steering commands received"])
                    result["status"] = "passed"
                    result["providerRecords"] = {"prompt": 1, "steer": 2}
                    (artifacts / "snapshot.json").write_text(json.dumps(snapshot(), ensure_ascii=False, indent=2))
                finally:
                    finish.touch()
                    terminate(core)
                    core = None
    except Exception as error:
        result["status"], result["error"] = "failed", str(error)
        raise
    finally:
        (artifacts / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/vantry"))
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--without-project", action="store_true")
    args = parser.parse_args()
    print(json.dumps(smoke(args.binary.resolve(), args.artifacts.resolve(), args.without_project), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
