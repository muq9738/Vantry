#!/usr/bin/env python3
"""Kill the real Vantry worker while Pi's detached bash tool is running."""
import argparse
from datetime import datetime
import fcntl
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
from zoneinfo import ZoneInfo

from smoke_desktop import get_json, terminate
from smoke_orchestration import await_condition


def run(binary, model, artifacts):
    artifacts.mkdir(parents=True, exist_ok=True)
    session = Path(tempfile.mkdtemp(prefix="vantry-worker-", dir="/tmp"))
    data, project = session / "data", session / "project"
    project.mkdir()
    report = {"status": "running", "model": model, "checks": [],
              "executedAt": datetime.now(ZoneInfo("Asia/Shanghai")).isoformat()}
    core, process_ids = None, []

    def control(*arguments):
        result = subprocess.run([str(binary), "ctl", "--data-dir", str(data), *arguments],
                                check=True, capture_output=True, text=True, timeout=15)
        return json.loads(result.stdout)

    def checkpoint(label):
        report["checks"].append(label)
        (artifacts / "smoke-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(label, flush=True)

    def process_gone(pid):
        try:
            os.kill(pid, 0)
            return False
        except ProcessLookupError:
            return True

    with (artifacts / "core.log").open("w") as log:
        try:
            core = subprocess.Popen([str(binary), "core", "--data-dir", str(data), "--workspace", str(project), "--model", model],
                                    stdout=log, stderr=subprocess.STDOUT)
            socket = data / "core.sock"
            await_condition("Core ready", lambda: get_json(socket, "/v1/health")["state"] == "ready", 15)
            thread = control("create", "--title", "worker crash recovery")
            message = "这是 worker 异常回收验收。仅用一次 bash 工具执行：printf '%s' \"$$\" > tool.pid; printf 'started' > started.txt; sleep 30; printf 'UNEXPECTED_ORPHAN_WRITE' > escaped.txt。完成后回复标记。不派发其他任务，不访问网络，不修改其他文件。"
            receipt = control("send", "--thread", thread["threadId"], "--message", message, "--command-id", "worker-crash-once")

            def tool_started():
                snapshot = control("status")
                run = next(run for run in snapshot["runs"] if run["id"] == receipt["runId"])
                if run["status"] == "failed":
                    raise AssertionError(run.get("error"))
                return run if (project / "started.txt").exists() and (project / "tool.pid").exists() else None

            running = await_condition("real Pi detached bash started", tool_started)
            job_dir = data / "jobs" / running["attemptId"]
            worker_pid = json.loads((job_dir / "worker.json").read_text())["pid"]
            pi_pid = json.loads((job_dir / "pi-process.json").read_text())["pid"]
            tool_pid = int((project / "tool.pid").read_text())
            process_ids = [pi_pid, tool_pid]
            report.update({"runId": running["id"], "attemptId": running["attemptId"],
                           "workerPid": worker_pid, "piPid": pi_pid, "toolPid": tool_pid})
            assert os.getpgid(pi_pid) == pi_pid and os.getpgid(tool_pid) == tool_pid and pi_pid != tool_pid
            checkpoint("Real Pi bash used its own detached process group")
            os.kill(worker_pid, signal.SIGKILL)

            def recovered():
                snapshot = control("status")
                run = next(run for run in snapshot["runs"] if run["id"] == running["id"])
                return run if run["status"] == "failed" else None

            final = await_condition("dead worker finalized after production cleanup", recovered, 20)
            assert "will not be resent" in final.get("error", ""), final
            await_condition("Pi and detached bash group exited", lambda: all(process_gone(pid) for pid in process_ids), 8)
            checkpoint("Production recovery terminated both Pi and its detached bash tool")
            workspace_fd = os.open(project, os.O_RDONLY)
            try:
                fcntl.flock(workspace_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                fcntl.flock(workspace_fd, fcntl.LOCK_UN)
            finally:
                os.close(workspace_fd)
            assert not (project / "escaped.txt").exists()
            checkpoint("Workspace was released after its previous writers exited")
            retry = control("send", "--thread", thread["threadId"], "--message", message, "--command-id", "worker-crash-once")
            assert retry == receipt
            entries = [json.loads(line) for line in (data / "sessions" / (thread["threadId"] + ".jsonl")).read_text().split("\n") if line.strip()]
            submissions = sum(entry.get("type") == "message" and entry.get("message", {}).get("role") == "user" for entry in entries)
            assert submissions == 1, submissions
            report["promptSubmissions"] = submissions
            checkpoint("The killed worker's submitted prompt was not resent")
            report["status"] = "passed"
        except Exception as error:
            report.update({"status": "failed", "error": str(error), "retainedSession": str(session)})
            raise
        finally:
            # Assertions above precede fallback cleanup; it cannot make a failed
            # production cleanup look like a passed check.
            if report["status"] != "passed":
                for pid in process_ids:
                    try:
                        os.killpg(pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
            terminate(core)
            (artifacts / "smoke-result.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
            if report["status"] == "passed":
                import shutil
                shutil.rmtree(session)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--artifacts", type=Path, default=Path(".agent/orchestration/worker-smoke"))
    arguments = parser.parse_args()
    run(arguments.binary.resolve(), arguments.model, arguments.artifacts.resolve())
