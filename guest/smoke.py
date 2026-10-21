#!/usr/bin/env python3
"""Opt-in integration smoke for an already booted, disposable Sapphire guest."""

import argparse
import datetime
import json
import os
import signal
import socket
import subprocess
import sys
import time
import uuid

ALPINE = "alpine@sha256:ae959799024a09da597284763c417a6e820e020fad0634c5267cf4f125e682bf"
HELLO = "hello-world@sha256:5099b89d7666cc2186cad769ddc262ddc7c335b33f5fe79f9ffe50a01282b23e"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--disposable-guest", action="store_true", required=True)
    parser.add_argument("--rpc-socket", required=True)
    parser.add_argument("--docker-socket", required=True)
    parser.add_argument("--forward-socket", required=True)
    parser.add_argument("--with-k3s", action="store_true")
    args = parser.parse_args()
    for path in (args.rpc_socket, args.docker_socket, args.forward_socket):
        if not os.path.isabs(path):
            parser.error("explicit absolute guest socket paths are required")
    marker = "sapphire-smoke-" + uuid.uuid4().hex[:10]
    machine, clone, volume, web = marker, marker + "-clone", marker + "-data", marker + "-web"
    ordinary = marker + "-ordinary"
    made_machines = []
    docker_started = False
    k3s_started = False
    volume_created = False
    web_created = False
    ordinary_created = False

    def interrupted(signum, _frame):
        raise KeyboardInterrupt(f"signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)

    def rpc(action, **fields):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(900)
            connection.connect(args.rpc_socket)
            connection.sendall((json.dumps({"action": action, **fields}) + "\n").encode())
            with connection.makefile("rb") as stream:
                line = stream.readline((4 << 20) + 1)
            if not line.endswith(b"\n") or len(line) > 4 << 20:
                raise RuntimeError("RPC reply missing or over wire limit")
            response = json.loads(line)
            if not response["ok"]:
                raise RuntimeError(f"{action}: {response.get('error', '')}")
            return response.get("result")

    def docker(*arguments, check=True):
        env = os.environ.copy()
        for key in ("DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
            env.pop(key, None)
        return subprocess.run(
            ["docker", "--host", "unix://" + args.docker_socket, *arguments],
            env=env, text=True, capture_output=True, check=check, timeout=300,
        ).stdout.strip()

    def execute(name, command, expected=0):
        result = rpc("machine.exec", name=name, arguments=["/bin/sh", "-c", command])
        if result["exitCode"] != expected:
            raise RuntimeError(f"{name}: exit {result['exitCode']}: {result['stderr']}")
        return result["stdout"]

    def cleanup(action):
        try:
            action()
        except Exception as error:
            print(f"cleanup: {error}", file=sys.stderr)

    try:
        status = rpc("status")
        assert status["protocol"] == 1 and status["pid"] == 1 and status["cgroupV2"], status
        guest_time = datetime.datetime.fromisoformat(status["wallClock"].replace("Z", "+00:00")).timestamp()
        assert status["clockValid"] and abs(time.time() - guest_time) < 120, status
        print("PASS guest PID 1, protocol, cgroup v2 and host-aligned wall clock", flush=True)
        config = dict(name=machine, distribution="alpine", cpus=1, memoryGiB=1, diskGiB=8,
                      docker=False, rosetta=False, shareHome=False)
        rpc("machine.create", configuration=config)
        made_machines.append(machine)
        info = rpc("machine.start", name=machine)
        assert info["status"] == "running" and info["memory"] == 1 << 30, info
        assert execute(machine, "uname -m").strip() == "aarch64"
        execute(machine, f"printf '%s' '{marker}' >/root/persistent")
        execute(machine, "exit 23", expected=23)
        machine_id = execute(machine, "cat /etc/machine-id")
        limits = execute(machine, "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/cpu.max /sys/fs/cgroup/pids.max")
        assert limits.splitlines() == ["1073741824", "100000 100000", "4096"], limits
        dio = execute(machine, """device=$(awk '$5 == "/" { print $3; exit }' /proc/self/mountinfo); cat "/sys/dev/block/$device/loop/dio" """)
        assert dio.strip() == "1", f"machine disk loop is not using direct I/O: {dio!r}"
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
            connection.settimeout(5)
            connection.connect(args.rpc_socket)
            connection.sendall((json.dumps({
                "action": "machine.exec", "name": machine,
                "arguments": ["/bin/sh", "-c", "sleep 600 & echo $$ $! >/root/cancel.pid; wait"],
            }) + "\n").encode())
            pids = []
            for _ in range(30):
                text = execute(machine, "cat /root/cancel.pid 2>/dev/null || true")
                if text.strip():
                    pids = [int(pid) for pid in text.split()]
                    break
                time.sleep(0.1)
            assert len(pids) == 2, "cancel test command never started"
        for _ in range(30):
            alive = execute(machine, " ".join(
                f"test -e /proc/{pid} && printf '{pid} ';"
                for pid in pids) + " true")
            if not alive:
                break
            time.sleep(0.1)
        assert not alive, f"disconnected exec left processes alive: {alive}"
        rpc("machine.stop", name=machine)
        rpc("machine.start", name=machine)
        assert execute(machine, "cat /root/persistent") == marker
        rpc("machine.stop", name=machine)
        rpc("machine.clone", name=machine, destination=clone)
        made_machines.append(clone)
        rpc("machine.start", name=clone)
        assert execute(clone, "cat /root/persistent") == marker
        assert execute(clone, "cat /etc/machine-id") != machine_id
        rpc("machine.stop", name=clone)
        print("PASS namespace machine, limits, exit code, disconnect cleanup, restart persistence and independent clone", flush=True)

        before = rpc("docker.status")
        docker_started = not before["running"]
        rpc("docker.start")
        assert "Hello from Docker!" in docker("run", "--rm", HELLO)
        docker("volume", "create", volume)
        volume_created = True
        docker("run", "-d", "--name", web, "--mount", f"type=volume,source={volume},target=/srv",
               "-p", "127.0.0.1::8080", ALPINE, "/bin/sh", "-c",
               f"printf '%s' '{marker}' >/srv/index.html; "
               "while :; do ("
               f"printf 'HTTP/1.0 200 OK\\r\\nContent-Length: {len(marker)}\\r\\nConnection: close\\r\\n\\r\\n'; "
               "cat /srv/index.html) | nc -l -p 8080 >/dev/null; done")
        web_created = True
        port = int(docker("inspect", "--format", '{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}', web))
        body = b""
        for attempt in range(30):
            try:
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                    connection.settimeout(3)
                    connection.connect(args.forward_socket)
                    connection.sendall((json.dumps({"host": "127.0.0.1", "port": port}) + "\n").encode()
                                       + b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                    chunks = []
                    while chunk := connection.recv(65536):
                        chunks.append(chunk)
                    body = b"".join(chunks)
                if b"200 OK" in body and marker.encode() in body:
                    break
            except OSError:
                pass
            time.sleep(0.2)
        assert b"200 OK" in body and marker.encode() in body, body
        docker("rm", "--force", web)
        web_created = False
        ordinary_created = True
        docker("run", "-d", "--name", ordinary, ALPINE, "sleep", "600")
        if docker_started:
            rpc("docker.stop")
            rpc("docker.start")
            assert docker("inspect", "--format", "{{.State.Running}}", ordinary) == "false"
            docker("start", ordinary)
        assert docker("run", "--rm", "--mount", f"type=volume,source={volume},target=/srv",
                      ALPINE, "cat", "/srv/index.html") == marker
        print("PASS Docker hello-world, localhost HTTP forwarding and named-volume persistence", flush=True)

        if args.with_k3s:
            before = rpc("k8s.status")
            k3s_started = not before["running"]
            ready = rpc("k8s.start")
            assert ready["ready"], ready
            assert "apiVersion: v1" in rpc("k8s.kubeconfig")
            if k3s_started:
                rpc("k8s.stop")
                k3s_started = False
                assert not docker("ps", "-a", "--filter", "label=io.kubernetes.pod.uid", "--format", "{{.ID}}")
                assert docker("inspect", "--format", "{{.State.Running}}", ordinary) == "true"
            print("PASS optional single-node Kubernetes readiness, kubeconfig and pod cleanup", flush=True)
        print(json.dumps({"result": "passed", "metrics": rpc("metrics")}, indent=2), flush=True)
    finally:
        if k3s_started:
            cleanup(lambda: rpc("k8s.stop"))
        for name in reversed(made_machines):
            cleanup(lambda name=name: rpc("machine.stop", name=name))
            cleanup(lambda name=name: rpc("machine.delete", name=name))
        if web_created:
            cleanup(lambda: docker("rm", "--force", web))
        if ordinary_created:
            cleanup(lambda: docker("rm", "--force", ordinary))
        if volume_created:
            cleanup(lambda: docker("volume", "rm", volume))
        if docker_started:
            cleanup(lambda: rpc("docker.stop"))


if __name__ == "__main__":
    main()
