#!/usr/bin/env python3
"""Exercise an isolated Sapphire VM without changing the host Docker context."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

PROJECT = Path(__file__).resolve().parent.parent
ALPINE = "alpine@sha256:ae959799024a09da597284763c417a6e820e020fad0634c5267cf4f125e682bf"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--engine", type=Path, default=PROJECT / "engine/build/sapphire-engine")
    parser.add_argument("--vmm", type=Path, default=PROJECT / "native/build/sapphire-vmm")
    parser.add_argument("--assets", type=Path, default=PROJECT / "guest/assets")
    parser.add_argument("--with-k8s", action="store_true")
    parser.add_argument(
        "--resource-seconds", type=int, default=0,
        help="also capture each workload's idle, 256 MiB tmpfs, and released phases (at least 10 seconds)",
    )
    parser.add_argument("--keep", action="store_true", help="retain the stopped test VM and logs on success")
    args = parser.parse_args()
    if args.resource_seconds and args.resource_seconds < 10:
        parser.error("--resource-seconds must be at least 10")
    if args.resource_seconds and not (PROJECT / "benchmarks/build/footprint").is_file():
        parser.error("build benchmarks/build/footprint before capturing resources")
    for tool in (args.engine, args.vmm):
        if not tool.is_file() or not os.access(tool, os.X_OK):
            parser.error(f"build the executable first: {tool}")
    if not shutil.which("docker"):
        parser.error("Docker CLI is required")
    if args.with_k8s and not shutil.which("kubectl"):
        parser.error("--with-k8s requires the host kubectl CLI")
    for name in ("kernel", "initrd.gz", "rootfs.img"):
        if not (args.assets / name).is_file():
            parser.error(f"build the independent guest assets first: {args.assets / name}")

    home = Path(tempfile.mkdtemp(prefix="sapphire-smoke-", dir="/tmp"))
    environment = dict(os.environ, SAPPHIRE_HOME=str(home), SAPPHIRE_VMM=str(args.vmm.resolve()))
    for key in ("DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_TLS", "DOCKER_TLS_CERTDIR"):
        environment.pop(key, None)
    docker_host = f"unix://{home}/docker.sock"
    environment["DOCKER_HOST"] = docker_host
    results = []
    passed = False
    started = False
    failure = None
    captures = []
    artifacts = {}
    engine_executable = args.engine.resolve()

    def run(arguments, timeout=120, check=True):
        result = subprocess.run(
            list(map(str, arguments)), env=environment, capture_output=True, text=True, timeout=timeout
        )
        if check and result.returncode:
            raise RuntimeError(f"{arguments[0]} failed ({result.returncode}): {(result.stderr or result.stdout)[-4000:]}")
        return result

    def engine(*arguments, timeout=120):
        return run([engine_executable, *arguments], timeout=timeout).stdout

    def call(action, timeout=120, **values):
        envelope = json.loads(engine("call", json.dumps(dict(action=action, **values)), timeout=timeout))
        if not envelope["ok"]:
            raise RuntimeError(f"{action}: {envelope.get('error')}")
        return envelope.get("result")

    def docker(*arguments, timeout=120):
        return run(["docker", "--host", docker_host, *arguments], timeout=timeout).stdout

    def check(label, action):
        before = time.monotonic()
        try:
            action()
        except (Exception, KeyboardInterrupt) as error:
            results.append({
                "check": label, "seconds": time.monotonic() - before,
                "passed": False, "error": f"{type(error).__name__}: {error}",
            })
            raise
        results.append({"check": label, "seconds": time.monotonic() - before, "passed": True})
        print(f"PASS {label}", flush=True)

    def profile(workload, allocate, release):
        if not args.resource_seconds:
            return
        specification = home / f"{workload}-resource-specification.json"
        specification.write_text(json.dumps({
            "workload": workload, "cpus": 2, "memoryMiB": 2048,
            "architecture": "arm64", "images": [ALPINE],
            "conditions": {
                "command": "sleeping workload; dd 256 x 1048576 bytes from /dev/zero into tmpfs; unlink",
                "cache": "warm, following functional checks",
                "rate": "no external client load",
                "phaseTransitions": "allocation and unlink complete before capture; ten-second settle",
                "scope": "sequential functional-smoke observation on a shared host",
                "kubernetesVersion": call("k8s.status")["version"] if workload == "k8s" else None,
                "machineRootTrust": "shared guest administrator" if workload == "machine" else None,
                "allocationBytes": 256 << 20, "tmpfsLimitBytes": 320 << 20,
                "workloadLimits": {
                    "memoryMiB": 1024 if workload == "machine" else 512,
                    "cpuQuota": "one CPU" if workload == "machine" else "shared VM ceiling",
                    "machineDiskGiB": 8 if workload == "machine" else None,
                },
                "priorChecks": [item["check"] for item in results if item["passed"]],
            },
        }, indent=2) + "\n")
        for phase in ("idle", "loaded", "reclaimed"):
            if phase == "loaded":
                allocate()
            elif phase == "reclaimed":
                release()
            time.sleep(10)
            status = json.loads(engine("status", "--json"))
            output = PROJECT / "benchmarks/results" / f"{home.name}-{workload}-{phase}.json"
            print(f"Measuring {workload}/{phase} for {args.resource_seconds}s", flush=True)
            run([
                "python3", PROJECT / "benchmarks/resources.py", "capture",
                "--backend", "sapphire", "--version",
                "experimental-" + "-".join(
                    f"{name}:{digest[:16]}" for name, digest in sorted(artifacts.items())
                ),
                "--pid", str(status["pid"]), "--workload", workload, "--phase", phase,
                "--specification", specification, "--seconds", str(args.resource_seconds),
                "--isolation", "shared", "--output", output,
            ], timeout=args.resource_seconds + 30)
            captures.append(str(output.relative_to(PROJECT)))

    def boot():
        nonlocal started
        # Mark ownership before starting so timeout/failure still attempts cleanup.
        started = True
        engine("start", "--cpus", "2", "--memory-mib", "2048", "--data-gib", "32", "--json", timeout=130)
        status = json.loads(engine("status", "--json"))
        assert status["running"] and status["backend"] == "hypervisor" and status["cpus"] == 2

    try:
        (home / "bin").mkdir(mode=0o700)
        for source, name in ((args.engine, "sapphire-engine"), (args.vmm, "sapphire-vmm")):
            destination = home / "bin" / name
            shutil.copy2(source, destination)
            artifacts[name] = hashlib.sha256(destination.read_bytes()).hexdigest()
        engine_executable = home / "bin/sapphire-engine"
        environment["SAPPHIRE_VMM"] = str(home / "bin/sapphire-vmm")
        (home / "assets").mkdir(mode=0o700)
        for name in ("kernel", "initrd.gz", "rootfs.img"):
            destination = home / "assets" / name
            shutil.copyfile(args.assets / name, destination)
            artifacts[name] = hashlib.sha256(destination.read_bytes()).hexdigest()
        check("direct Hypervisor VM and guest RPC readiness", boot)
        check("on-demand Docker readiness", lambda: call("docker.start", timeout=150))

        def container():
            output = docker("run", "--rm", ALPINE, "/bin/sh", "-c", "printf 'sapphire\\n'; uname -m", timeout=180)
            assert output.splitlines() == ["sapphire", "aarch64"], output
        check("ARM64 container execution and outbound image pull", container)

        def container_resources():
            if not args.resource_seconds:
                return
            name = "sapphire-resource"
            docker("run", "-d", "--name", name, "--tmpfs", "/memory:size=320m",
                   "--memory=512m", ALPINE, "sleep", "86400")
            try:
                profile("container",
                        lambda: docker("exec", name, "dd", "if=/dev/zero", "of=/memory/data",
                                       "bs=1048576", "count=256"),
                        lambda: docker("exec", name, "rm", "/memory/data"))
            finally:
                docker("rm", "-f", name)
        if args.resource_seconds:
            check("container memory allocation and release observations", container_resources)

        def volume():
            docker("volume", "create", "sapphire-smoke-data")
            docker("run", "--rm", "-v", "sapphire-smoke-data:/data", ALPINE, "sh", "-c", "printf durable >/data/marker")
            assert docker("run", "--rm", "-v", "sapphire-smoke-data:/data", ALPINE, "cat", "/data/marker") == "durable"
        check("named-volume persistence", volume)

        def published_port():
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            docker(
                "run", "-d", "--name", "sapphire-smoke-http", "-p", f"127.0.0.1:{port}:80",
                ALPINE, "sh", "-c",
                "while :; do printf 'HTTP/1.0 200 OK\\r\\nContent-Length: 14\\r\\n"
                "Connection: close\\r\\n\\r\\nsapphire-http\\n' | nc -l -p 80 >/dev/null; done",
            )
            deadline = time.monotonic() + 20
            while True:
                try:
                    with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=2) as response:
                        assert response.read() == b"sapphire-http\n"
                    break
                except (OSError, urllib.error.URLError) as error:
                    if time.monotonic() >= deadline:
                        raise RuntimeError(
                            f"published HTTP did not become ready: {error}; "
                            f"container logs: {docker('logs', 'sapphire-smoke-http')[-2000:]}"
                        ) from error
                    time.sleep(.25)
            docker("rm", "-f", "sapphire-smoke-http")
            deadline = time.monotonic() + 5
            while any(item["hostPort"] == port for item in json.loads(engine("status", "--json"))["forwards"]):
                if time.monotonic() >= deadline:
                    raise RuntimeError("published listener remained after container removal")
                time.sleep(.1)
        check("event-driven published TCP forwarding", published_port)

        def inventory():
            result = call("docker.inventory")
            assert any(volume["name"] == "sapphire-smoke-data" for volume in result["volumes"])
            assert result["images"] and isinstance(result["projects"], list)
        check("direct Docker API inventory", inventory)

        def compose():
            configuration = home / "compose.yaml"
            configuration.write_text(
                "services:\n  worker:\n    image: " + ALPINE +
                "\n    command: [sh, -c, 'echo sapphire-compose; sleep 600']\n"
            )
            prefix = ["compose", "-f", str(configuration), "-p", "sapphire-smoke"]
            docker(*prefix, "up", "-d")
            projects = call("docker.inventory")["projects"]
            assert any(project["Name"] == "sapphire-smoke" for project in projects), projects
            assert "sapphire-compose" in docker(*prefix, "logs", "--no-log-prefix", "worker")
            docker(*prefix, "down", "--volumes")
        check("Compose service and project lifecycle", compose)

        def machine():
            call("machine.create", timeout=180, configuration={
                "name": "smoke", "distribution": "alpine", "cpus": 1, "memoryGiB": 1, "diskGiB": 8,
                "docker": False, "rosetta": False, "shareHome": False,
            })
            call("machine.start", name="smoke")
            result = call("machine.exec", name="smoke", arguments=["sh", "-c", "printf machine >/root/marker; uname -m"])
            assert result["exitCode"] == 0 and result["stdout"].strip() == "aarch64", result
            result = call("machine.exec", name="smoke", arguments=[
                "sh", "-c", "cat /sys/fs/cgroup/cpu.max; cat /sys/fs/cgroup/memory.max",
            ])
            lines = result["stdout"].splitlines()
            assert result["exitCode"] == 0 and len(lines) == 2, result
            quota, period = map(int, lines[0].split())
            assert quota == period and int(lines[1]) == 1 << 30, result
            call("machine.stop", name="smoke")
            call("machine.start", name="smoke")
            result = call("machine.exec", name="smoke", arguments=["cat", "/root/marker"])
            assert result["exitCode"] == 0 and result["stdout"] == "machine", result
            if args.resource_seconds:
                def machine_exec(*arguments):
                    result = call("machine.exec", name="smoke", arguments=list(arguments))
                    assert result["exitCode"] == 0, result
                machine_exec("mkdir", "-p", "/run/memory")
                machine_exec("mount", "-t", "tmpfs", "-o", "size=320m", "tmpfs", "/run/memory")
                try:
                    profile("machine",
                            lambda: machine_exec("dd", "if=/dev/zero", "of=/run/memory/data",
                                                 "bs=1048576", "count=256"),
                            lambda: machine_exec("rm", "/run/memory/data"))
                finally:
                    machine_exec("umount", "/run/memory")
            call("machine.stop", name="smoke")
        check("machine namespaces, enforced cgroup limits, and persistence", machine)

        def docker_stop():
            name = "sapphire-stop-test"
            docker("run", "-d", "--name", name, ALPINE, "sleep", "86400")
            call("docker.stop")
            assert not call("docker.status")["running"]
            call("docker.start", timeout=150)
            assert docker("inspect", "--format", "{{.State.Running}}", name).strip() == "false"
            docker("rm", name)
        check("Docker stop drains workloads and allows restart", docker_stop)

        if args.with_k8s:
            def kubernetes():
                survivor = "sapphire-non-k8s"
                docker("run", "-d", "--name", survivor, ALPINE, "sleep", "86400")
                call("k8s.start", timeout=240)
                configuration = call("k8s.kubeconfig")
                kubeconfig = home / "kubeconfig"
                kubeconfig.write_text(configuration)
                kubeconfig.chmod(0o600)
                prefix = ["kubectl", "--kubeconfig", str(kubeconfig)]
                run(prefix + ["wait", "--for=create", "node/sapphire", "--timeout=120s"], timeout=135)
                run(prefix + ["wait", "--for=condition=Ready", "nodes", "--all", "--timeout=120s"], timeout=135)
                run(prefix + ["wait", "--for=create", "serviceaccount/default", "--timeout=120s"], timeout=135)
                for deployment in ("coredns", "local-path-provisioner", "metrics-server"):
                    run(prefix + ["wait", "-n", "kube-system", "--for=create",
                                  "deployment/" + deployment, "--timeout=120s"], timeout=135)
                    run(prefix + ["rollout", "status", "-n", "kube-system",
                                  "deployment/" + deployment, "--timeout=120s"], timeout=135)
                run(prefix + ["run", "sapphire-smoke", "--restart=Never", "--image=" + ALPINE, "--", "sh", "-c",
                              "nslookup kubernetes.default.svc.cluster.local >/dev/null && echo sapphire-k8s"])
                run(prefix + ["wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/sapphire-smoke", "--timeout=120s"], timeout=135)
                assert run(prefix + ["logs", "sapphire-smoke"]).stdout.strip() == "sapphire-k8s"
                run(prefix + ["delete", "pod", "sapphire-smoke", "--wait=true"], timeout=90)
                if args.resource_seconds:
                    manifest = home / "resource-pod.json"
                    manifest.write_text(json.dumps({
                        "apiVersion": "v1", "kind": "Pod",
                        "metadata": {"name": "sapphire-resource"},
                        "spec": {
                            "restartPolicy": "Never",
                            "containers": [{
                                "name": "memory", "image": ALPINE, "command": ["sleep", "86400"],
                                "resources": {"limits": {"memory": "512Mi"}},
                                "volumeMounts": [{"name": "memory", "mountPath": "/memory"}],
                            }],
                            "volumes": [{"name": "memory", "emptyDir": {
                                "medium": "Memory", "sizeLimit": "320Mi",
                            }}],
                        },
                    }) + "\n")
                    run(prefix + ["apply", "-f", str(manifest)])
                    run(prefix + ["wait", "--for=condition=Ready", "pod/sapphire-resource",
                                  "--timeout=120s"], timeout=135)
                    try:
                        profile("k8s",
                                lambda: run(prefix + ["exec", "sapphire-resource", "--", "dd",
                                                       "if=/dev/zero", "of=/memory/data",
                                                       "bs=1048576", "count=256"]),
                                lambda: run(prefix + ["exec", "sapphire-resource", "--",
                                                       "rm", "/memory/data"]))
                    finally:
                        run(prefix + ["delete", "pod", "sapphire-resource", "--wait=true"], timeout=90)
                call("k8s.stop")
                status = call("k8s.status")
                assert not status.get("running", False), status
                assert not docker("ps", "-a", "--filter", "label=io.kubernetes.pod.uid", "--format", "{{.ID}}").strip()
                assert docker("inspect", "--format", "{{.State.Running}}", survivor).strip() == "true"
                docker("rm", "-f", survivor)
            check("Kubernetes readiness, service DNS, workload, logs, and stop", kubernetes)

        check("orderly VM shutdown", lambda: engine("stop", timeout=75))
        started = False
        check("same-data-directory restart", boot)
        check("Docker restart", lambda: call("docker.start", timeout=150))
        assert docker("run", "--rm", "-v", "sapphire-smoke-data:/data", ALPINE, "cat", "/data/marker") == "durable"
        call("machine.start", name="smoke")
        result = call("machine.exec", name="smoke", arguments=["cat", "/root/marker"])
        assert result["exitCode"] == 0 and result["stdout"] == "machine", result
        results.append({"check": "container and machine data survive VM restart", "passed": True})
        passed = True
    except (Exception, KeyboardInterrupt) as error:
        failure = f"{type(error).__name__}: {error}"
        print(f"FAIL {failure}", flush=True)
    finally:
        if started:
            try:
                stopped = run([engine_executable, "stop"], timeout=75, check=False)
                stopped_cleanly = stopped.returncode == 0
            except (OSError, subprocess.TimeoutExpired) as error:
                stopped_cleanly = False
                print(f"Stopping the test VM failed: {error}", flush=True)
            if not stopped_cleanly:
                passed = False
                print("Sapphire test VM did not stop cleanly; retained for diagnosis.", flush=True)
        record = json.dumps({
            "passed": passed, "withKubernetes": args.with_k8s, "checks": results,
            "error": failure, "resourceCaptures": captures,
            "artifacts": artifacts,
            "resourceParity": "not measured by this functional smoke test",
        }, indent=2) + "\n"
        (home / "validation.json").write_text(record)
        destination = PROJECT / "benchmarks/results" / f"functional-{home.name}.json"
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(record)
        print(f"Validation record: {destination}", flush=True)
        if passed and not args.keep:
            shutil.rmtree(home)
        else:
            print(f"Retained isolated Sapphire runtime: {home}", flush=True)
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
