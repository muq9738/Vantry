#!/usr/bin/env python3
"""Verify real Pi execution across native UI close and Core SIGKILL/restart.

Uses an isolated temporary Git repository and the user's existing Pi setup.
No credentials are read by this script. Result artifacts contain only this
script's constrained test prompts, process IDs, and committed Vantry state.
"""
import argparse
from datetime import datetime
from zoneinfo import ZoneInfo
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from smoke_desktop import Automation, get_json, terminate


def await_condition(label, predicate, timeout=180):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            value = predicate()
            if value:
                return value
        except (OSError, ValueError, KeyError, ConnectionError) as error:
            last_error = error
        time.sleep(0.15)
    raise AssertionError(f"Timed out: {label}; last error: {last_error}")


def run(binary, model, artifacts):
    artifacts.mkdir(parents=True, exist_ok=True)
    report = {"status": "running", "model": model, "checks": [],
              "executedAt": datetime.now(ZoneInfo("Asia/Shanghai")).isoformat(), "executor": "Codex / Pi"}
    processes, clients, logs = [], [], []
    # Retain failed test workspaces for diagnosis; successful runs are removed.
    session = Path(tempfile.mkdtemp(prefix="vantry-real-", dir="/tmp"))
    data, repository, automation = session / "data", session / "repo", session / "ui.sock"
    repository.mkdir()
    (repository / "README.md").write_text("Vantry real provider recovery fixture.\n")
    for arguments in (["git", "init", "-q"], ["git", "add", "README.md"],
                      ["git", "-c", "user.name=Vantry smoke", "-c", "user.email=smoke@localhost", "commit", "-qm", "Prepare recovery fixture"]):
        subprocess.run(arguments, cwd=repository, check=True, capture_output=True)
    environment = dict(os.environ)
    environment.pop("MYGO_AUTOMATION", None)
    socket = data / "core.sock"
    marker = "VANTRY_CHILD_RECOVERY_OK"

    def checkpoint(label):
        report["checks"].append(label)
        (artifacts / "smoke-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(label, flush=True)

    def start_core():
        log = (artifacts / f"core-{len(processes)}.log").open("w")
        logs.append(log)
        process = subprocess.Popen([str(binary), "core", "--data-dir", str(data), "--provider", "pi",
                                    "--model", model, "--workspace", str(repository)],
                                   env=environment, stdout=log, stderr=subprocess.STDOUT)
        processes.append(process)
        await_condition("Core ready", lambda: get_json(socket, "/v1/health")["state"] == "ready", 20)
        return process

    def open_window():
        log = (artifacts / f"desktop-{len(processes)}.log").open("w")
        logs.append(log)
        ui_environment = dict(environment, MYGO_AUTOMATION=str(automation))
        process = subprocess.Popen([str(binary), "desktop", "--data-dir", str(data), "--attach-only",
                                    "--provider", "pi", "--model", model, "--workspace", str(repository)],
                                   env=ui_environment, stdout=log, stderr=subprocess.STDOUT)
        processes.append(process)
        await_condition("native automation", automation.exists, 20)
        client = Automation(automation, process_id=process.pid)
        clients.append(client)
        await_condition("native content window", lambda: client.call("windows")["windows"], 20)
        await_condition("native controls", lambda: client.control("新会话"), 20)
        return process, client

    try:
        core = start_core()
        ui, client = open_window()
        client.press("新会话")
        client.fill("你是恢复验收的父任务。现在仅回复 PARENT_READY，不调用工具，不派发任务。后续如果收到子任务结果，请包含其中 VANTRY_CHILD_RECOVERY_OK 标记，并回复 PARENT_RESUMED_OK；不要再次派发子任务。")
        client.press("发送")

        def parent_ready():
            snapshot = get_json(socket, "/v1/snapshot")
            return next((run for run in snapshot["runs"] if run["status"] == "completed" and "PARENT_READY" in run["output"]), None)

        parent = await_condition("real parent reply", parent_ready)
        checkpoint("Real Pi parent completed through the native Send button")
        # The long tool creates an observable start before Core is killed.
        client.fill("这是进程恢复验收。仅用 bash 工具执行一次以下命令：printf 'CHILD_STARTED' > started.txt; sleep 18; printf 'VANTRY_CHILD_RECOVERY_OK' > result.txt。命令完成后读取 result.txt 并最终回复其完整标记。不派发其他任务，不访问网络，不修改其他文件。")
        client.press("派出子任务")

        def child_dispatched():
            snapshot = get_json(socket, "/v1/snapshot")
            return next((task for task in snapshot.get("tasks", []) if task["parentRunId"] == parent["id"]), None)

        task = await_condition("persisted child task", child_dispatched)
        checkpoint("Native Delegate button created a persisted Vantry child task")
        client.capture(artifacts / "ui-dispatched.png")
        client.close()
        clients.remove(client)
        terminate(ui)
        await_condition("UI automation removed", lambda: not automation.exists(), 10)
        assert get_json(socket, "/v1/health")["state"] == "ready"
        checkpoint("Closing the native UI left Core and the child execution alive")
        started = await_condition("real child bash started in its worktree", lambda: next(data.rglob("started.txt"), None))
        assert started.parent != repository, "child unexpectedly wrote the main checkout"
        before = get_json(socket, "/v1/snapshot")
        child = next(run for run in before["runs"] if run["id"] == task["childRunId"])
        assert child["status"] == "running", child
        report.update({"parentRunId": parent["id"], "childRunId": child["id"], "childAttemptId": child["attemptId"],
                       "corePidBefore": core.pid, "childWorkspace": str(started.parent)})
        core.kill()
        core.wait(timeout=5)
        checkpoint("SIGKILL stopped Core while the real child bash command was running")
        result_file = started.parent / "result.txt"
        await_condition("worker completed its tool while Core was dead", result_file.exists, 60)
        assert result_file.read_text() == marker
        checkpoint("Independent worker wrote the expected result with Core absent")
        restarted = start_core()
        report["corePidAfter"] = restarted.pid

        def delivered_and_resumed():
            snapshot = get_json(socket, "/v1/snapshot")
            recovered_child = next(run for run in snapshot["runs"] if run["id"] == child["id"])
            recovered_parent = next(run for run in snapshot["runs"] if run["id"] == parent["id"])
            if recovered_child["status"] == "failed":
                raise AssertionError(f"Recovered child failed: {recovered_child.get('error')}")
            if recovered_parent["status"] == "failed":
                raise AssertionError(f"Parent continuation failed: {recovered_parent.get('error')}")
            cohort = next((cohort for cohort in snapshot.get("cohorts", []) if cohort["parentRunId"] == parent["id"]
                           and cohort.get("continuationAttemptId")), None)
            if recovered_child["status"] == "completed" and recovered_parent["status"] == "completed" and cohort \
                    and marker in recovered_parent["output"] and "PARENT_RESUMED_OK" in recovered_parent["output"]:
                return snapshot

        final = await_condition("child delivery and automatic parent continuation", delivered_and_resumed)
        final_task = next(task for task in final["tasks"] if task["id"] == task_id(final, child["id"]))
        final_child = next(run for run in final["runs"] if run["id"] == child["id"])
        assert final_child["attemptId"] == child["attemptId"], "restart replaced the submitted child Attempt"
        session_path = data / "sessions" / (final_task["childThreadId"] + ".jsonl")
        session_entries = [json.loads(line) for line in session_path.read_text().splitlines() if line.strip()]
        submissions = sum(entry.get("type") == "message" and entry.get("message", {}).get("role") == "user" for entry in session_entries)
        assert submissions == 1, f"child prompt was submitted {submissions} times"
        report["childPromptSubmissions"] = submissions
        assert final_task["deliveryStatus"] == "acknowledged", final_task
        cohort = next(cohort for cohort in final["cohorts"] if cohort["parentRunId"] == parent["id"])
        assert cohort["status"] == "acknowledged", cohort
        (artifacts / "snapshot-after-recovery.json").write_text(json.dumps(final, ensure_ascii=False, indent=2))
        checkpoint("Restart reattached the same child Attempt, delivered its result and completed the parent continuation")
        duplicate = subprocess.run([str(binary), "ctl", "--data-dir", str(data), "ack", "--parent-run", parent["id"],
                                    "--generation", str(cohort["generation"]), "--delivery", cohort["deliveryMessageId"]],
                                   check=True, text=True, capture_output=True)
        assert json.loads(duplicate.stdout)["accepted"]
        second = get_json(socket, "/v1/snapshot")
        assert len(second["runs"]) == len(final["runs"])
        assert second["cohorts"] == final["cohorts"]
        checkpoint("Repeated acknowledgement did not create a second parent continuation")
        # A second restart must also leave consumed deliveries settled.
        restarted.kill()
        restarted.wait(timeout=5)
        start_core()
        stable = get_json(socket, "/v1/snapshot")
        assert stable["cohorts"] == final["cohorts"]
        assert len(stable["runs"]) == len(final["runs"])
        checkpoint("A second Core restart preserved the consumed delivery and continuation identity")
        ui, client = open_window()
        await_condition("restored parent thread", lambda: client.control("PARENT_READY") or any("PARENT_RESUMED_OK" in node.get("label", "") for node in client.tree()), 20)
        # Navigation may select the newer child; select parent using its first instruction.
        parent_thread = next(thread for thread in stable["threads"] if thread["id"] == parent["threadId"])
        report["parentThreadId"] = parent_thread["id"]
        client.capture(artifacts / "ui-restored.png")
        checkpoint("Reopened native UI loaded the persisted task and conversation state")
        report["status"] = "passed"
        report["childResult"] = marker
        report["continuationAttemptId"] = cohort["continuationAttemptId"]
    except Exception as error:
        report["status"], report["error"], report["retainedSession"] = "failed", str(error), str(session)
        if clients:
            try:
                clients[-1].capture(artifacts / "ui-failure.png")
                (artifacts / "ui-tree-failure.json").write_text(json.dumps(clients[-1].tree(), ensure_ascii=False, indent=2))
            except Exception:
                pass
        raise
    finally:
        for client in clients:
            client.close()
        for process in reversed(processes):
            terminate(process)
        for log in logs:
            log.close()
        (artifacts / "smoke-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        if report["status"] == "passed":
            import shutil
            shutil.rmtree(session)


def task_id(snapshot, child_run_id):
    return next(task["id"] for task in snapshot["tasks"] if task["childRunId"] == child_run_id)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--artifacts", type=Path, default=Path(".agent/orchestration/real-smoke"))
    arguments = parser.parse_args()
    run(arguments.binary.resolve(), arguments.model, arguments.artifacts.resolve())
