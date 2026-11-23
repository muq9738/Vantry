#!/usr/bin/env python3
"""Exercise a real MyGo test build against a temporary, independent Core.

Build first with: go build -tags e2e -o bin/vantry-e2e ./cmd/vantry
The ordinary product build contains no native automation listener.
"""

import argparse
import base64
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("vantry.local", timeout=2)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def get_json(path, endpoint):
    connection = UnixHTTP(path)
    try:
        connection.request("GET", endpoint)
        response = connection.getresponse()
        value = json.load(response)
        if response.status != 200:
            raise RuntimeError(f"{endpoint}: HTTP {response.status}: {value}")
        return value
    finally:
        connection.close()


def wait_until(description, check, timeout=10):
    deadline = time.monotonic() + timeout
    last_error = None
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (OSError, http.client.HTTPException) as error:
            last_error = error
        time.sleep(0.025)
    raise AssertionError(f"Timed out waiting for {description}; last error: {last_error}")


class Automation:
    def __init__(self, path, process_id=None):
        self.process_id = process_id
        self.connection = socket.socket(socket.AF_UNIX)
        self.connection.settimeout(4)
        self.connection.connect(str(path))
        self.reader = self.connection.makefile("rb")
        self.sequence = 0

    def close(self):
        self.reader.close()
        self.connection.close()

    def call(self, method, params=None):
        self.sequence += 1
        request = {"id": self.sequence, "method": method}
        if params is not None:
            request["params"] = params
        self.connection.sendall(json.dumps(request, ensure_ascii=False).encode() + b"\n")
        line = self.reader.readline()
        if not line:
            raise RuntimeError(f"Automation closed while running {method}")
        response = json.loads(line)
        assert response["id"] == self.sequence, response
        if "error" in response:
            raise RuntimeError(f"Automation {method}: {response['error']}")
        return response["result"]

    def tree(self):
        return self.call("tree")["nodes"]

    def control(self, label, role="button"):
        return next((node for node in self.tree() if node.get("role") == role
                     and node.get("label") == label and "disabled" not in node.get("states", [])), None)

    def activate(self):
        # macOS pauses the native surface while the window is fully occluded.
        # Bring the UI under test to the front as a user would, then act.
        if self.process_id is None or sys.platform != "darwin":
            return
        script = ('ObjC.import("AppKit"); var a = $.NSRunningApplication'
                  f'.runningApplicationWithProcessIdentifier({self.process_id}); '
                  'if (!a.active) a.activateWithOptions($.NSApplicationActivateIgnoringOtherApps);')
        subprocess.run(["/usr/bin/osascript", "-l", "JavaScript", "-e", script],
                       check=True, capture_output=True, timeout=5)

    def press(self, label, role="button"):
        self.activate()
        control = wait_until(f"enabled {label} {role}", lambda: self.control(label, role))
        self.call("act", {"id": control["id"], "action": "press"})

    def fill(self, text):
        self.activate()
        field = wait_until("message input", lambda: self.control("消息输入框", "textField"))
        self.call("act", {"id": field["id"], "action": "focus"})
        self.call("idle", {"timeout": 2000})
        # New Session replaces the input element. Resolve its current identity
        # after focus/layout settles instead of typing into a retiring node.
        field = wait_until("focused message input", lambda: self.control("消息输入框", "textField"))
        self.call("act", {"id": field["id"], "action": "setValue", "text": text})
        self.call("idle", {"timeout": 2000})

    def capture(self, path):
        self.activate()
        result = self.call("capture")
        image = base64.b64decode(result["png"], validate=True)
        assert image.startswith(b"\x89PNG\r\n\x1a\n"), "capture is not PNG"
        assert result["width"] >= 640 and result["height"] >= 420, result
        path.write_bytes(image)
        return {"path": str(path), "width": result["width"], "height": result["height"]}


def terminate(process):
    if process is not None and process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)
            raise AssertionError("Application did not close cooperatively")


def smoke(binary, artifacts):
    artifacts.mkdir(parents=True, exist_ok=True)
    result = {"checks": [], "captures": []}
    processes = []
    clients = []
    with tempfile.TemporaryDirectory(prefix="vantry-ui-", dir="/tmp") as session:
        session = Path(session)
        data = session / "data"
        automation_path = session / "ui.sock"
        core_socket = data / "core.sock"
        environment = dict(os.environ)
        environment.pop("MYGO_AUTOMATION", None)
        with (artifacts / "core.log").open("w") as core_log, (artifacts / "desktop.log").open("w") as ui_log:
            try:
                core = subprocess.Popen([str(binary), "core", "--data-dir", str(data),
                                         "--controlled-delay", "150ms", "--provider", "controlled"], env=environment,
                                        stdout=core_log, stderr=subprocess.STDOUT)
                processes.append(core)
                wait_until("Core ready", lambda: get_json(core_socket, "/v1/health")["state"] == "ready")
                ui_environment = dict(environment, MYGO_AUTOMATION=str(automation_path))

                def open_window():
                    process = subprocess.Popen([str(binary), "desktop", "--data-dir", str(data), "--attach-only", "--provider", "controlled"],
                                               env=ui_environment, stdout=ui_log, stderr=subprocess.STDOUT)
                    processes.append(process)
                    wait_until("native automation socket", automation_path.exists)
                    client = Automation(automation_path, process_id=process.pid)
                    clients.append(client)
                    windows = wait_until("visible native window", lambda: client.call("windows")["windows"])
                    assert windows[0]["kind"] == "content" and windows[0]["title"] == "Vantry", windows
                    assert windows[0]["visible"], windows
                    if sys.platform == "darwin":
                        # macOS may pause frames while another application
                        # occludes the test window. Exercise the visible app,
                        # without forcing frames through capture in assertions.
                        subprocess.run([
                            "osascript", "-e",
                            f'tell application "System Events" to set frontmost of '
                            f'(first process whose unix id is {process.pid}) to true',
                        ], check=True, capture_output=True, timeout=5)
                        wait_until("foreground native window", lambda: any(
                            window["focused"] for window in client.call("windows")["windows"]))
                    wait_until("native controls", lambda: client.control("新会话"))
                    return process, client

                ui, client = open_window()
                result["checks"].append("real MyGo native content window")
                result["captures"].append(client.capture(artifacts / "ui-empty.png"))
                client.press("新会话")
                wait_until("editable local draft after New Session", lambda: client.control("消息输入框", "textField"))
                assert get_json(core_socket, "/v1/snapshot")["threads"] == []
                result["checks"].append("new session stays a local draft until first send")
                message = "你好，Vantry。请检查中文输入、流式输出与任务停止。\n第二行：保留真实多行输入。" * 3
                client.fill(message)
                client.press("发送")
                wait_until("committed thread after first send", lambda: len(get_json(core_socket, "/v1/snapshot")["threads"]) == 1)

                def streaming_run():
                    runs = get_json(core_socket, "/v1/snapshot")["runs"]
                    return next((run for run in runs if run["input"] == message
                                 and run["status"] == "running" and run["output"]), None)

                running = wait_until("persisted Chinese input and streamed output", streaming_run)
                result["checks"].extend(["Chinese multiline text entered through native TextArea", "streamed output committed by Core"])
                result["captures"].append(client.capture(artifacts / "ui-streaming.png"))
                client.press("停止")
                wait_until("cancelled Run", lambda: any(run["id"] == running["id"] and run["status"] == "cancelled"
                                                       for run in get_json(core_socket, "/v1/snapshot")["runs"]))
                result["checks"].append("native stop button cancels the active Run")
                client.fill("完成一次完整回复。")
                client.press("发送")
                wait_until("completed reply", lambda: any(run["input"] == "完成一次完整回复。" and run["status"] == "completed"
                                                         for run in get_json(core_socket, "/v1/snapshot")["runs"]))
                wait_until("completed response rendered", lambda: any("任务已完成" in node.get("label", "")
                                                                   for node in client.tree()))
                result["checks"].append("second Run reaches completed through real IPC")
                result["captures"].append(client.capture(artifacts / "ui-conversation.png"))
                (artifacts / "ui-tree.json").write_text(json.dumps(client.tree(), ensure_ascii=False, indent=2))
                draft = "重开后保留的未发送草稿。\n第二行仍然保留。"
                client.fill(draft)
                wait_until("draft saved to private state file", lambda: (data / "desktop-state.json").exists()
                           and draft in json.loads((data / "desktop-state.json").read_text()).get("drafts", {}).values())
                client.close()
                clients.remove(client)
                terminate(ui)
                assert ui.returncode == 0, f"desktop exited with {ui.returncode}"
                assert get_json(core_socket, "/v1/health")["state"] == "ready"
                retained = get_json(core_socket, "/v1/snapshot")
                assert len(retained["threads"]) == 1 and len(retained["runs"]) == 2, retained
                result["checks"].append("closing desktop leaves independent Core and history alive")
                wait_until("automation socket removed", lambda: not automation_path.exists())
                restored_ui, client = open_window()
                wait_until("history rendered after reopening", lambda: any("任务已完成" in node.get("label", "")
                                                                        for node in client.tree()))
                result["captures"].append(client.capture(artifacts / "ui-restored.png"))
                result["checks"].append("reopened native window restores committed conversation")
                wait_until("unsent multiline draft restored", lambda: client.control("消息输入框", "textField").get("value") == draft)
                assert len(get_json(core_socket, "/v1/snapshot")["runs"]) == 2, "draft was submitted during restoration"
                result["checks"].append("reopened native window restores unsent multiline draft without submitting it")
                (artifacts / "snapshot.json").write_text(json.dumps(retained, ensure_ascii=False, indent=2))
                result["status"] = "passed"
            except Exception as error:
                result["status"] = "failed"
                result["error"] = str(error)
                if clients:
                    try:
                        (artifacts / "ui-tree.json").write_text(
                            json.dumps(clients[-1].tree(), ensure_ascii=False, indent=2))
                        clients[-1].capture(artifacts / "ui-failure.png")
                    except Exception:
                        pass
                raise
            finally:
                for client in clients:
                    client.close()
                for process in reversed(processes):
                    terminate(process)
                (artifacts / "smoke-result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("bin/vantry-e2e"))
    parser.add_argument("--artifacts", type=Path, required=True)
    args = parser.parse_args()
    result = smoke(args.binary.resolve(), args.artifacts.resolve())
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
