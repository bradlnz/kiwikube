#!/usr/bin/env python3
"""Run after ./build.sh. Real PTY, isolated fake kubectl/SSH; no cluster writes."""
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time

ROOT = Path(__file__).resolve().parents[1]
FAKE = r'''#!/usr/bin/env python3
import json, os, pathlib, sys, time, datetime
import http.server, socketserver, urllib.parse
args = sys.argv[1:]
with open(os.environ["SMOKE_TRACE"], "a") as f:
    f.write(json.dumps([pathlib.Path(sys.argv[0]).name] + args) + "\n")
if pathlib.Path(sys.argv[0]).name == "wl-copy":
    pathlib.Path(os.environ["SMOKE_CLIPBOARD"]).write_bytes(sys.stdin.buffer.read())
    sys.exit(0)
if "edit" in args:
    state = pathlib.Path(os.environ["SMOKE_EDITED"])
    if state.exists():
        print("Forbidden: policy denied this edit", flush=True)
        sys.exit(1)
    print("EDITOR_READY", flush=True)
    if sys.stdin.readline().strip() == "save":
        state.write_text("saved")
        print("secret/app-secret edited", flush=True)
    sys.exit(0)
if "proxy" in args:
    if "second-cluster" in args:
        time.sleep(0.4)  # Keep the switching spinner visible during proxy startup.
    fixture = json.loads(pathlib.Path(os.environ["SMOKE_FIXTURE"]).read_text())["items"]
    for item in fixture:
        item["metadata"].setdefault("uid", item["kind"] + "-" + item["metadata"]["name"])
        item["metadata"]["resourceVersion"] = "1"
        if item["kind"] == "Event":
            ago = datetime.timedelta(minutes=2 if item["type"] == "Warning" else 120)
            item["lastTimestamp"] = (datetime.datetime.now(datetime.timezone.utc) - ago).isoformat()
        for status, spec in zip(item.get("status", {}).get("containerStatuses", []), item.get("spec", {}).get("containers", [])):
            status["name"] = spec["name"]
    kinds = {"namespaces":"Namespace","pods":"Pod","services":"Service","deployments":"Deployment","statefulsets":"StatefulSet","daemonsets":"DaemonSet","jobs":"Job","cronjobs":"CronJob","ingresses":"Ingress","persistentvolumeclaims":"PersistentVolumeClaim","events":"Event","secrets":"Secret","configmaps":"ConfigMap","nodes":"Node"}
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_GET(self):
            request = urllib.parse.urlsplit(self.path)
            query = urllib.parse.parse_qs(request.query)
            parts = request.path.split("/")
            if parts[-1] == "cronjobs":
                self.send_error(403, "Forbidden: cronjobs cannot be listed")
                return
            if parts[-1] == "summary" and pathlib.Path(os.environ["SMOKE_DENY_STATS"]).exists():
                self.send_error(403, "Forbidden: node stats denied")
                return
            if "metrics.k8s.io" in parts and parts[-1] == "worker-1":
                self.send_error(403, "Forbidden: pod metrics denied")
                return
            self.send_response(200)
            self.end_headers()
            try:
                if parts[-1] == "summary":
                    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
                    counter = int(time.monotonic()*1000000)
                    stats = {"node":{"nodeName":"node-a","cpu":{"time":now,"usageNanoCores":2000000000},"memory":{"time":now,"workingSetBytes":8388608},"fs":{"usedBytes":750000,"capacityBytes":1000000},"network":{"time":now,"name":"eth0","rxBytes":counter,"txBytes":counter*2}}}
                    self.wfile.write(json.dumps(stats).encode())
                    return
                if "metrics.k8s.io" in parts:
                    metrics = {"metadata":{"name":parts[-1],"namespace":parts[-3]},"timestamp":datetime.datetime.now(datetime.timezone.utc).isoformat(),"containers":[{"usage":{"cpu":"250m","memory":"2Mi"}}]}
                    self.wfile.write(json.dumps(metrics).encode())
                    return
                if query.get("watch") == ["true"]:
                    self.wfile.flush()
                    time.sleep(300)
                elif parts[-1] == "log":
                    if query.get("previous") == ["true"]:
                        self.wfile.write(b"2026-09-07T00:00:00Z captured previous instance\n")
                    else:
                        for i in range(10000):
                            self.wfile.write(("2026-09-07T00:00:01Z captured-log-%d\n" % i).encode())
                            self.wfile.flush()
                            time.sleep(0.03)
                elif len(parts) == 7 and parts[5] == "pods":
                    item = next(i for i in fixture if i["kind"] == "Pod" and i["metadata"]["name"] == parts[6])
                    self.wfile.write(json.dumps(item).encode())
                else:
                    kind = kinds.get(parts[-1])
                    self.wfile.write(json.dumps({"kind":kind+"List", "metadata":{"resourceVersion":"1"}, "items":[i for i in fixture if i["kind"] == kind]}).encode())
            except (BrokenPipeError, ConnectionResetError): pass
    class Server(socketserver.ThreadingUnixStreamServer):
        daemon_threads = True
    socket_path = next(a.split("=", 1)[1] for a in args if a.startswith("--unix-socket="))
    Server(socket_path, Handler).serve_forever()
elif pathlib.Path(sys.argv[0]).name == "ssh" or "exec" in args:
    print("SHELL_READY", flush=True)
    for line in sys.stdin:
        if line.strip() == "exit": break
        print("SHELL_ECHO:" + line.strip(), flush=True)
elif "get-contexts" in args:
    print("fixture-cluster\nsecond-cluster\nthird-cluster")
elif "current-context" in args:
    print("fixture-cluster")
elif "logs" in args:
    if "--previous" in args:
        print("PREVIOUS_LOG", flush=True)
    else:
        for i in range(10000):
            print("2026-09-07T00:00:00Z %s log-entry-%d" % (["INFO", "WARN", "ERROR", "DEBUG"][i % 4], i), flush=True)
            time.sleep(0.03)
elif "get" in args and "json" in args:
    resource = args[args.index("get") + 1]
    if resource.startswith(("secrets/", "configmaps/")):
        kind = "Secret" if resource.startswith("secrets/") else "ConfigMap"
        fixture = json.loads(pathlib.Path(os.environ["SMOKE_FIXTURE"]).read_text())["items"]
        item = next(i for i in fixture if i["kind"] == kind)
        if kind == "Secret" and pathlib.Path(os.environ["SMOKE_EDITED"]).exists():
            item["data"]["token"] = "ZWRpdGVkLXRva2VuCg=="
        print(json.dumps(item))
        sys.exit(0)
    if resource == "cronjobs":
        print("Forbidden: cronjobs cannot be listed", file=sys.stderr)
        sys.exit(1)
    print(pathlib.Path(os.environ["SMOKE_FIXTURE"]).read_text())
elif "describe" in args:
    if "nodes/node-a" in args:
        print("Name: node-a\nRoles: worker\nLabels: kubernetes.io/hostname=node-a\nCreationTimestamp: Mon, 07 Sep 2026 00:00:00 +0000\nConditions:\n  Type    Status  Reason\n  ----    ------  ------\n  Ready   True    KubeletReady\nAddresses:\n  InternalIP: 10.0.0.10\nSystem Info:\n  OS Image: Fixture Linux\n  Kernel Version: 6.18\nEvents: <none>")
    else:
        print("Name: api-1\nStatus: Running\nContainers:\n  api:\n    Image: example.test/api:v1\n    Ready: True\nEvents: started")
elif "history" in args:
    print("REVISION CHANGE-CAUSE\n7 release")
else:
    print("kind: Pod\nmetadata:\n  name: api-1")
'''


def main():
    with tempfile.TemporaryDirectory(prefix="kiwikube-smoke-") as tmp:
        tmp = Path(tmp)
        for command in ("kubectl", "ssh", "wl-copy"):
            target = tmp / command
            target.write_text(FAKE)
            target.chmod(0o755)
        trace = tmp / "trace.jsonl"
        env = dict(os.environ, PATH=f"{tmp}:{os.environ['PATH']}",
                   SMOKE_TRACE=str(trace), SMOKE_DENY_STATS=str(tmp / "deny-stats"), SMOKE_CLIPBOARD=str(tmp / "clipboard"), SMOKE_EDITED=str(tmp / "edited"), SMOKE_FIXTURE=str(ROOT / "src/testdata/cluster.json"))
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 140, 0, 0))
        original = termios.tcgetattr(slave)
        proc = subprocess.Popen([os.environ.get("KIWIKUBE_BIN", str(ROOT / "kiwikube")), "--config", str(tmp / "config.json"), "--refresh", "2", "--archive-dir", str(tmp / "archive")],
                                stdin=slave, stdout=slave, stderr=slave, env=env, start_new_session=True)
        output = bytearray()

        def wait_for(text, start=0):
            deadline = time.monotonic() + 6
            while time.monotonic() < deadline:
                if text.encode() in output[start:]:
                    return
                if select.select([master], [], [], 0.1)[0]:
                    output.extend(os.read(master, 65536))
                if proc.poll() is not None:
                    break
            raise AssertionError(f"Missing {text!r}: {output[-3000:]!r}")

        def send(keys, expected):
            start = len(output)
            os.write(master, keys.encode())
            wait_for(expected, start)

        def click_label(row, label, expected):
            footer = re.findall(f"\x1b\\[{row};1H(.*?)\x1b\\[K".encode(), output, re.S)[-1]
            plain = re.sub(rb"\x1b\[[0-9;]*m", b"", footer).decode()
            col = plain.index(label) + 2
            send(f"\x1b[<0;{col};{row}M", expected)

        def click_footer(label, expected):
            click_label(32, label, expected)

        try:
            wait_for("api-1")
            wait_for("PARTIAL")
            send("\x06", "Search · pods")
            send("api\r", "/api")
            send("\x1b", "worker-1")
            send("\x1b[<0;30;5M", "production / worker-1")
            send("\x1b[<0;30;4M", "default / api-1")
            click_footer("m metrics", "POD METRICS")
            wait_for("0.250 cores")
            send("\x1b[<0;30;5M", "Metrics unavailable:")
            wait_for("403")
            send("\x1b[<0;30;4M", "0.250 cores")
            send("\x1b[<0;138;3M", "RESTARTS")
            send("\x1b[<0;2;4M", "node-a")
            send("\r", "Live node stats")
            wait_for("2.00 / 8.00 cores")
            wait_for("Fixture Linux")
            wait_for("KubeletReady")
            wait_for("observed peak")
            click_label(2, "Resources", "Enter stats")
            click_label(2, "Node:", "2.00 / 8.00 cores")
            (tmp / "deny-stats").write_text("denied")
            wait_for("Stats unavailable:", len(output))
            (tmp / "deny-stats").unlink()
            send("r", "Live node stats")
            send("\x1b", "Enter stats")
            send("\x1b[<0;2;6M", "api-1")
            send("\r", "log-entry-")
            wait_for("ERROR log-entry-")
            assert re.search(rb"\x1b\[48;5;234;38;5;210m [^\x1b]*ERROR log-entry-", output)
            click_label(2, "Resources", "Enter logs")
            click_label(2, "Logs:", "log-entry-")
            click_footer("c container", "sidecar")
            click_footer("p previous", "PREVIOUS_LOG")
            send("F", "api.example.test/")
            click_label(2, "×", "Resources")  # Close inactive Logs, keep Flow selected.
            click_label(2, "Resources", "Enter logs")
            shell_start=len(output)
            click_footer("s shell", "SHELL_READY")
            wait_for("Ctrl+] close",shell_start)
            assert b"\x1b[?1049l" not in output[shell_start:], "pod shell left the dashboard"
            typing_start=len(output)
            send("hello\n", "SHELL_ECHO:hello")
            assert b"NAMESPACES" not in output[typing_start:], "typing repainted the dashboard over the shell"
            send("exit\n", "Session ended")
            click_footer("s shell", "SHELL_READY")
            send("\x1d", "Session ended")
            send("S", "SHELL_READY")
            send("exit\n", "Session ended")
            send("d", "Running")
            wait_for("╭ Overview")
            wait_for("Containers")
            click_label(2, "Resources", "Enter logs")
            click_label(2, "Describe:", "Running")
            send("\x06Image\r", "example.test/api:v1")
            send("\x1b", "NAMESPACES")
            send("\x1b[<0;2;10M", "migration")
            send("\r", "log-entry-")
            send("\x1b[<0;2;15M", "[T Time:")
            click_label(2, "Logs:", "log-entry-")
            send("r", "log-entry-")  # Reload still targets the Job after browsing Events.
            send("\x1b", "Enter info")
            click_label(32, "[T Time:", "24h")
            send("\x1b[<0;4;27M", "events · 1")
            click_label(32, "[K Type:", "Warning")
            send("\x1b[B\r", "[K Type: Warning")
            click_label(32, "[K Type:", "Normal")
            send("\x1b[B\r", "No matching resources")
            click_label(32, "[X Reset]", "events · 2")
            send("\x1b[<0;2;16M", "app-secret")
            send("\r", "demo-only-token")
            click_label(27, "c Copy", "Copied token")
            assert (tmp / "clipboard").read_bytes() == b"demo-only-token"
            click_label(27, "e Edit", "EDITOR_READY")
            send("save\n", "edited-token")
            send("c", "Copied token")
            assert (tmp / "clipboard").read_bytes() == b"edited-token\n"
            send("e", "Edit failed:")
            assert b"Forbidden: policy denied" in output
            send("c", "Copied token")
            assert (tmp / "clipboard").read_bytes() == b"edited-token\n"
            click_label(27, "Esc close", "app-secret")
            send("\x1b[<0;2;17M", "app-config")
            send("\r", "app.yaml")
            assert b"\x1b[48;5;234;38;5;222mtrue" in output, "missing configuration syntax colors"
            click_label(27, "Esc close", "app-config")
            send("\x1b[<0;2;6M", "api-1")
            send("\x1b[<0;14;1M", "Flow graph")
            send("\x1b[B\x1b[B\r", "api.example.test/")
            wait_for("api:80")
            click_label(2, "Resources", "Enter logs")
            click_label(2, "Flow", "api.example.test/")
            click_label(2, "×", "Enter logs")
            send("t]w", "Saved")
            send("A", "Permanent archive")
            send("\x06captured\r", "captured-log-")
            send("\r", "Archived record")
            send("\x1b", "Permanent archive")
            send("\x1b", "NAMESPACES")
            settings = json.loads((tmp / "config.json").read_text())
            assert settings["theme"] == "ocean" and settings["sidebar_width"] == 28
            # Alt+2 switches contexts even from inside a viewer; then click slot 1.
            send("A", "Permanent archive")
            switch_start = len(output)
            send("\x1b2", "Switching cluster")
            wait_for("KiwiKube / second-cluster", switch_start)
            header = output.index(b"KiwiKube / second-cluster", switch_start)
            wait_for("worker-1", header)
            switching = output[switch_start:header]
            assert b"\x1b[?1049l" not in switching, "switching left the dashboard"
            assert "⠋".encode() in switching and "⠙".encode() in switching, "spinner did not animate"
            # At 140 columns, five numeric 3-column slots begin at column 126.
            switch_start = len(output)
            send("\x1b[<0;127;1M", "Switching cluster")
            wait_for("KiwiKube / fixture-cluster", switch_start)
            header = output.index(b"KiwiKube / fixture-cluster", switch_start)
            wait_for("worker-1", header)
            # Compact terminals must still expose the namespace tree when it has focus.
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 18, 50, 0, 0))
            proc.send_signal(signal.SIGWINCH)
            send("h", "All namespaces")
            send("?", "NAVIGATE")
            send("q", "All namespaces")
            os.write(master, b"q")
            proc.wait(timeout=5)
            assert proc.returncode == 0
            assert termios.tcgetattr(slave) == original, "terminal mode was not restored"
            # A foreground Ctrl+C must stop the collector and let SQLite flush cleanly.
            headless = subprocess.Popen([os.environ.get("KIWIKUBE_BIN", str(ROOT / "kiwikube")), "--config", str(tmp / "config.json"), "--context", "fixture-cluster", "--collect"], env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, start_new_session=True)
            try:
                assert select.select([headless.stderr], [], [], 6)[0], "headless startup timed out"
                assert b"Recording" in headless.stderr.readline()
                time.sleep(0.4)
                os.killpg(headless.pid, signal.SIGINT)
                headless.wait(timeout=5)
                assert headless.returncode == 0, headless.stderr.read()
            finally:
                if headless.poll() is None:
                    headless.kill()
                    headless.wait()
                headless.stderr.close()
            export = subprocess.check_output([os.environ.get("KIWIKUBE_BIN", str(ROOT / "kiwikube")), "--config", str(tmp / "config.json"), "--context", "fixture-cluster", "--history", "--archive-kind", "logs"], env=env)
            stored = [json.loads(line) for line in export.splitlines()]
            assert stored and any("captured" in row["message"] for row in stored)
            secrets = subprocess.check_output([os.environ.get("KIWIKUBE_BIN", str(ROOT / "kiwikube")), "--config", str(tmp / "config.json"), "--context", "fixture-cluster", "--history", "--archive-kind", "secrets"], env=env)
            assert not secrets.strip(), "displayed Secrets must not be archived"
            calls = [json.loads(line) for line in trace.read_text().splitlines()]
            copies = [call for call in calls if call[0] == "wl-copy"]
            assert len(copies) == 3 and all("--sensitive" in call for call in copies)
            edits = [call for call in calls if "edit" in call]
            assert len(edits) == 2 and all(call[call.index("edit") + 1] == "secrets/app-secret" and "--context" in call for call in edits)
            shells = [call for call in calls if "exec" in call]
            assert shells and shells[0][-2:] == ["--", "/bin/sh"]
            job_logs = [call for call in calls if "logs" in call and "job/migration" in call]
            assert len(job_logs) == 2 and all("--all-pods=true" in call and "--all-containers=true" in call for call in job_logs)
            assert any(call[0] == "ssh" and call[-1] == "192.0.2.10" for call in calls)
            assert all("--context" in call for call in calls if "get" in call or "logs" in call)
            print("PASS: shell modal, formatted descriptions and node details, namespace colors, node gauges and permission errors, pod metrics panel, top menus, Flow tab, search modal, event dropdowns, exact Secret copy, edit success/policy denial, pod/Job log tabs, colors, shell/SSH, resize, terminal restoration, archive without Secrets, switching spinner")
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            os.close(master)
            os.close(slave)


if __name__ == "__main__":
    main()
