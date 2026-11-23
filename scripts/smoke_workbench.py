#!/usr/bin/env python3
"""Exercise model discovery, approval, queue, steer and diff with real Pi.

Only a temporary Git repository and private Core directory are used. Pi uses
its existing installation and authentication. No credentials are read here.
"""
import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

from smoke_desktop import Automation, UnixHTTP, get_json, terminate
from smoke_orchestration import await_condition


def post_json(socket, endpoint, payload):
    connection = UnixHTTP(socket)
    try:
        connection.request("POST", endpoint, json.dumps(payload), {"Content-Type": "application/json"})
        response = connection.getresponse()
        value = json.load(response)
        assert response.status == 200, value
        return value
    finally:
        connection.close()


def run(binary, model, artifacts, native=False, queue_model=""):
    artifacts.mkdir(parents=True, exist_ok=True)
    session = Path(tempfile.mkdtemp(prefix="vantry-workbench-", dir="/tmp"))
    data, repository = session / "data", session / "repo"
    repository.mkdir()
    (repository / "README.md").write_text("Vantry workbench fixture.\n")
    for arguments in (["git", "init", "-q"], ["git", "add", "."],
                      ["git", "-c", "user.name=Vantry smoke", "-c", "user.email=smoke@localhost", "commit", "-qm", "fixture"]):
        subprocess.run(arguments, cwd=repository, capture_output=True, check=True)
    socket = data / "core.sock"
    processes, logs, clients = [], [], []
    report = {"status": "running", "model": model, "checks": [],
              "executedAt": datetime.now().astimezone().isoformat()}

    def checkpoint(label):
        report["checks"].append(label)
        state_path = data / "desktop-state.json"
        if native and state_path.exists():
            report.setdefault("uiSelections", []).append({"check": label, "selected": json.loads(state_path.read_text()).get("selected")})
        (artifacts / "smoke-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(label, flush=True)

    def ctl(*arguments):
        completed = subprocess.run([str(binary), "ctl", "--data-dir", str(data), *arguments],
                                   capture_output=True, text=True, timeout=25)
        if completed.returncode:
            raise AssertionError(f"ctl {arguments[0]}: {completed.stderr.strip()}")
        return json.loads(completed.stdout)

    def snapshot():
        return get_json(socket, "/v1/snapshot")

    def start_core():
        log = (artifacts / f"core-{len(processes)}.log").open("w")
        logs.append(log)
        process = subprocess.Popen([str(binary), "core", "--data-dir", str(data), "--provider", "pi",
                                    "--model", model, "--workspace", str(repository)],
                                   stdout=log, stderr=subprocess.STDOUT,
                                   env={k: v for k, v in os.environ.items() if k != "MYGO_AUTOMATION"})
        processes.append(process)
        await_condition("Core ready", lambda: get_json(socket, "/v1/health")["state"] == "ready", 20)
        return process

    def open_window():
        automation = session / "ui.sock"
        log = (artifacts / "desktop.log").open("w")
        logs.append(log)
        ui = subprocess.Popen([str(binary), "desktop", "--data-dir", str(data), "--attach-only",
                               "--model", model, "--workspace", str(repository)],
                              stdout=log, stderr=subprocess.STDOUT,
                              env=dict(os.environ, MYGO_AUTOMATION=str(automation)))
        processes.append(ui)
        await_condition("native automation socket", automation.exists, 20)
        client = Automation(automation, process_id=ui.pid)
        clients.append(client)
        await_condition("native window visible", lambda: client.call("windows")["windows"], 20)
        await_condition("native request rendered", lambda: client.control("允许执行"), 20)
        return client

    try:
        core = start_core()
        catalog_result = ctl("models")
        models = catalog_result.get("catalog", {}).get("models", []) if isinstance(catalog_result, dict) else catalog_result
        for item in models:
            provider = item.get("provider", "")
            if provider and not item["id"].startswith(provider + "/"):
                item["id"] = provider + "/" + item["id"]
        assert any(item["id"] == model for item in models), "configured model absent from real catalog"
        checkpoint("Real Pi model catalog loaded without submitting a prompt")
        thread = ctl("create", "--thread", "workbench-smoke", "--title", "工作台闭环验收",
                     "--workspace", str(repository), "--model", model, "--approval", "ask")["threadId"]
        first = ctl("send", "--thread", thread, "--command-id", "first-prompt", "--message",
                    "这是隔离工作区的自动化验收。只调用一次 bash，原样执行："
                    "printf 'RUNNING' > started.txt; sleep 12; printf 'WORKBENCH_DONE' > result.txt。"
                    "工具完成后直接回复 WORKBENCH_DONE 和后续用户要求的标记。"
                    "不要调用其他工具，不派子任务，不访问网络，不改其他文件。")

        def pending_request():
            state = snapshot()
            current = next(item for item in state["runs"] if item["id"] == first["runId"])
            assert current["status"] not in ("failed", "interrupted"), current
            return next((item for item in state.get("requests", [])
                         if item["runId"] == first["runId"] and item["status"] == "pending"), None)

        request = await_condition("actual tool confirmation", pending_request)
        assert request["method"] == "confirm", request
        assert not (repository / "started.txt").exists(), "tool ran before confirmation"
        client = open_window() if native else None
        if client:
            client.capture(artifacts / "ui-approval.png")
            client.fill("THIS_MUST_NEVER_RUN")
            client.press("排队")
            removed = await_condition("native queue committed", lambda: next((item for item in snapshot()["inputs"]
                                      if item["message"] == "THIS_MUST_NEVER_RUN"), None))
            client.press("查看队列")
            client.press("取消排队")
            await_condition("native queue cancelled", lambda: next(item for item in snapshot()["inputs"]
                            if item["id"] == removed["id"])["status"] == "cancelled")
            checkpoint("Native Queue and Cancel buttons changed the durable queue")
        if queue_model:
            choice = next(item for item in models if item["id"] == queue_model)
            if client:
                client.press("选择模型")
                await_condition("model choice loaded", lambda: client.control(choice["name"]), 20)
                client.press(choice["name"])
                await_condition("model picker closed", lambda: not any(node.get("label") == "搜索模型" for node in client.tree()), 20)
            else:
                post_json(socket, "/v1/commands", {"version": 1, "commandId": "model-next", "kind": "thread.model", "threadId": thread, "model": queue_model})
            await_condition("thread model changed", lambda: next(item for item in snapshot()["threads"] if item["id"] == thread)["model"] == queue_model)
            assert next(item for item in snapshot()["runs"] if item["id"] == first["runId"])["model"] == model
            report["queuedModel"] = queue_model
            checkpoint("Model picker changes subsequent messages while the active Run keeps its original model")
        queued = ctl("queue", "--thread", thread, "--command-id", "queue-next", "--message",
                     "不要调用工具。只回复 QUEUE_DONE。")
        duplicate = ctl("queue", "--thread", thread, "--command-id", "queue-next", "--message",
                        "不要调用工具。只回复 QUEUE_DONE。")
        assert duplicate == queued, "queue retry changed receipt"
        if not client:
            removed = ctl("queue", "--thread", thread, "--message", "THIS_MUST_NEVER_RUN")
            ctl("cancel-input", "--input", removed["inputId"])
            assert next(item for item in snapshot()["inputs"] if item["id"] == removed["inputId"])["status"] == "cancelled"
        checkpoint("Real tool waits for approval; queued input is idempotent and removable")
        core.kill()
        core.wait(timeout=5)
        core = start_core()
        restored = next(item for item in snapshot()["requests"] if item["id"] == request["id"])
        assert restored["status"] == "pending"
        assert not (repository / "started.txt").exists()
        checkpoint("Core restart preserves the same pending request and queue without running the tool")
        if client:
            client.press("允许执行")
        else:
            response = ctl("respond", "--request", request["id"], "--confirm", "--command-id", "approve-once")
            assert response == ctl("respond", "--request", request["id"], "--confirm", "--command-id", "approve-once")
        await_condition("approved tool started", lambda: (repository / "started.txt").exists())
        running = next(item for item in snapshot()["runs"] if item["id"] == first["runId"])
        assert running["status"] == "running", running
        steer_text = "等正在运行的命令完成后，最终回复必须额外包含 STEER_ACCEPTED。不要调用新工具或派发任务。"
        if client:
            client.fill(steer_text)
            client.press("运行中发送方式", "menuButton")
            client.press("调整当前任务", "menuItemRadio")
            client.press("Steer")
            native_input = await_condition("native Steer committed", lambda: next((item for item in snapshot()["inputs"]
                                           if item["kind"] == "steer" and item["message"] == steer_text), None))
            steering = {"inputId": native_input["id"]}
        else:
            steering = ctl("steer", "--thread", thread, "--run", running["id"], "--attempt", running["attemptId"],
                           "--command-id", "steer-once", "--message", steer_text)

        def steer_delivered():
            item = next(item for item in snapshot()["inputs"] if item["id"] == steering["inputId"])
            assert item["status"] != "failed", item
            return item["status"] == "delivered"

        await_condition("Pi accepted live steer", steer_delivered)
        checkpoint("Approval reaches the real worker and Steer is accepted during its bash tool")

        def completed():
            state = snapshot()
            initial = next(item for item in state["runs"] if item["id"] == first["runId"])
            assert initial["status"] not in ("failed", "interrupted"), initial
            queued_input = next(item for item in state["inputs"] if item["id"] == queued["inputId"])
            following = next((item for item in state["runs"] if item["id"] == queued_input.get("runId")), None)
            if following:
                assert following["status"] not in ("failed", "interrupted"), following
            if initial["status"] == "completed" and following and following["status"] == "completed":
                assert "STEER_ACCEPTED" in initial["output"], initial
                assert "QUEUE_DONE" in following["output"], following
                if queue_model:
                    assert following["model"] == queue_model, following
                return state

        final = await_condition("first prompt and queued prompt completed", completed)
        assert len(final["runs"]) == 2, "cancelled/duplicate queued input created another run"
        assert (repository / "result.txt").read_text() == "WORKBENCH_DONE"
        tools = [tool for tool in final["tools"] if tool["runId"] == first["runId"]]
        assert any(tool["name"] == "bash" and tool["status"] == "completed" for tool in tools), tools
        assert next(item for item in final["requests"] if item["id"] == request["id"])["status"] == "responded"
        checkpoint("Real tool result is visible and the queued prompt starts once after settlement")
        changes = ctl("diff", "--thread", thread)
        assert Path(changes["path"]).resolve() == repository.resolve()
        assert changes["isGit"] and "WORKBENCH_DONE" in changes["diff"]
        checkpoint("Workspace query shows actual model-written files and their contents")
        if client:
            client.press("工作区")
            await_condition("native workspace rendered", lambda: any(node.get("label") == "result.txt" for node in client.tree()) and client.control("刷新"), 20)
            client.capture(artifacts / "ui-workspace.png")
            client.press("查看全部变更")
            await_condition("native diff rendered", lambda: any("WORKBENCH_DONE" in node.get("label", "") for node in client.tree()) and client.control("关闭变更"), 20)
            client.capture(artifacts / "ui-diff.png")
            client.press("关闭变更")
            (artifacts / "ui-tree.json").write_text(json.dumps(client.tree(), ensure_ascii=False, indent=2))
            checkpoint("Native approval, Steer and workspace panel connect to real Pi/Core outcomes")
        core.kill()
        core.wait(timeout=5)
        start_core()
        stable = snapshot()
        assert len(stable["runs"]) == 2
        assert stable["inputs"] == final["inputs"]
        assert stable["tools"] == final["tools"]
        assert stable["requests"] == final["requests"]
        checkpoint("A second restart preserves queue, tool and approval outcomes without replay")
        report.update(status="passed", threadId=thread, runCount=len(stable["runs"]))
        (artifacts / "snapshot.json").write_text(json.dumps(stable, ensure_ascii=False, indent=2))
    except Exception as error:
        report.update(status="failed", error=str(error), retainedSession=str(session))
        if clients:
            try:
                clients[-1].capture(artifacts / "ui-failure.png")
                (artifacts / "ui-failure-tree.json").write_text(json.dumps(clients[-1].tree(), ensure_ascii=False, indent=2))
            except Exception:
                pass
        try:
            (artifacts / "snapshot-failure.json").write_text(json.dumps(snapshot(), ensure_ascii=False, indent=2))
        except Exception:
            pass
        raise
    finally:
        try:
            for item in snapshot()["runs"]:
                if item["status"] in ("queued", "running", "waiting_children"):
                    ctl("stop", "--thread", item["threadId"], "--run", item["id"])
        except Exception:
            pass
        for client in clients:
            client.close()
        for process in reversed(processes):
            terminate(process)
        for log in logs:
            log.close()
        (artifacts / "smoke-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        if report["status"] == "passed":
            shutil.rmtree(session)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--artifacts", type=Path, default=Path(".agent/workbench/real-smoke"))
    parser.add_argument("--native", action="store_true", help="Exercise native controls; binary must be built with -tags e2e")
    parser.add_argument("--queue-model", default="", help="Switch to this model for the queued message while the original Run is active")
    arguments = parser.parse_args()
    run(arguments.binary.resolve(), arguments.model, arguments.artifacts.resolve(), arguments.native, arguments.queue_model)
