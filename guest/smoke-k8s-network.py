#!/usr/bin/env python3
"""Check DNS, ClusterIP traffic and network policy in an owned disposable VM."""

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import uuid

ALPINE = "alpine@sha256:ae959799024a09da597284763c417a6e820e020fad0634c5267cf4f125e682bf"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--disposable-guest", action="store_true", required=True)
    parser.add_argument("--home", type=Path, required=True)
    args = parser.parse_args()
    home = args.home.resolve()
    engine_path = home / "bin/sapphire-engine"
    if not engine_path.is_file() or not (home / "control.sock").is_socket():
        parser.error("start the owned disposable VM first; its copied engine and control socket are required")
    environment = dict(os.environ, SAPPHIRE_HOME=str(home))
    for key in ("DOCKER_CONTEXT", "DOCKER_HOST", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
        environment.pop(key, None)
    namespace = "sapphire-netcheck-" + uuid.uuid4().hex[:10]
    machine = namespace + "-idle"
    ordinary = namespace + "-ordinary"
    kubeconfig = home / "network-check-kubeconfig"
    created, machine_created, ordinary_created, passed = False, False, False, False

    def interrupted(signum, _frame):
        raise KeyboardInterrupt(f"signal {signum}")

    signal.signal(signal.SIGTERM, interrupted)

    def run(command, timeout=120, data=None):
        result = subprocess.run(
            command, env=environment, input=data, text=True,
            capture_output=True, timeout=timeout,
        )
        if result.returncode:
            raise RuntimeError(f"{command[0]}: {(result.stdout + result.stderr)[-6000:]}")
        return result.stdout

    def engine(*arguments, timeout=240):
        return run([str(engine_path), *arguments], timeout=timeout)

    def docker(*arguments):
        return run(["docker", "--host", "unix://" + str(home / "docker.sock"), *arguments]).strip()

    def call(action, **fields):
        response = json.loads(engine("call", json.dumps(dict(action=action, **fields))))
        if not response["ok"]:
            raise RuntimeError(f"{action}: {response.get('error', '')}")
        return response["result"]

    def kube(*arguments, data=None, timeout=120):
        return run(
            ["kubectl", "--kubeconfig", str(kubeconfig), "--request-timeout=100s", *arguments],
            data=data, timeout=timeout,
        )

    def apply(*objects):
        kube("apply", "-f", "-", data=json.dumps(dict(apiVersion="v1", kind="List", items=objects)))

    def network_diagnostics():
        script = """
set -eu
test "$(cat /proc/sys/net/bridge/bridge-nf-call-iptables)" = 1
printf 'PASS bridge-nf-call-iptables=1\\n'
chain=SAPPHIRE-NETCHECK
iptables -N "$chain"
trap 'iptables -F "$chain"; iptables -X "$chain"' EXIT
iptables -A "$chain" -m mark ! --mark 0x10000/0x10000 -j NFLOG --nflog-group 100 -m limit --limit 10/minute --limit-burst 10
iptables -A "$chain" -m mark ! --mark 0x10000/0x10000 -j REJECT
iptables -A "$chain" -j MARK --set-xmark 0x10000/0x10000
printf 'PASS policy MARK, mark, NFLOG, limit and REJECT kernel operations\\n'
zcat /proc/config.gz | grep '^CONFIG_NF_CT_NETLINK=y$'
if grep -nE 'Aborting sync.*iptables-restore|Failed to list conntrack entries|Failed to execute iptables-restore' /var/log/sapphire/k3s.log; then
    exit 1
fi
printf 'PASS no kube-router restore or kube-proxy conntrack cleanup errors\\n'
"""
        print(run([
            "docker", "--host", "unix://" + str(home / "docker.sock"),
            "run", "--rm", "--pid=host", "--network=host",
            "--cap-add", "NET_ADMIN", "--cap-add", "SYS_PTRACE", ALPINE,
            "chroot", "/proc/1/root", "/bin/sh", "-ec", script,
        ], timeout=30), flush=True)

    def client(name, command):
        apply(dict(
            apiVersion="v1", kind="Pod",
            metadata=dict(name=name, namespace=namespace, labels=dict(role="client")),
            spec=dict(restartPolicy="Never", containers=[dict(
                name="client", image=ALPINE, command=["/bin/sh", "-ec", command],
                resources=dict(limits=dict(memory="64Mi", cpu="100m")),
            )]),
        ))
        try:
            kube("-n", namespace, "wait", "--for=jsonpath={.status.phase}=Succeeded",
                 "pod/" + name, "--timeout=90s", timeout=100)
        finally:
            print(kube("-n", namespace, "logs", name), flush=True)

    try:
        machine_created = True
        call("machine.create", configuration=dict(
            name=machine, distribution="alpine", cpus=1, memoryGiB=1, diskGiB=8,
            docker=False, rosetta=False, shareHome=False,
        ))
        info = call("machine.start", name=machine)
        if info["initSystem"] != "sapphire-init":
            raise RuntimeError(f"Alpine minirootfs selected unusable init: {info['initSystem']}")
        idle = call("machine.exec", name=machine, arguments=["/bin/sh", "-ec", """
sleep 5
tr '\\0' ' ' </proc/1/cmdline
for file in /proc/[0-9]*/comm; do
    IFS= read -r command <"$file" || continue
    case "$command" in init|getty|agetty|openrc) exit 1;; esac
done
"""])
        if idle["exitCode"] or "__machine-init" not in idle["stdout"]:
            raise RuntimeError(f"Alpine fallback init check failed: {idle}")
        call("machine.stop", name=machine)
        call("machine.delete", name=machine)
        machine_created = False
        print("PASS Alpine fallback PID 1 with no OpenRC/getty respawns", flush=True)
        ready = call("k8s.start")
        if not ready["ready"]:
            raise RuntimeError(f"Kubernetes start did not report ready: {ready}")
        descriptor = os.open(kubeconfig, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            stream.write(call("k8s.kubeconfig"))
        for deployment in ("coredns", "local-path-provisioner", "metrics-server"):
            kube("-n", "kube-system", "rollout", "status", "deployment/" + deployment,
                 "--timeout=150s", timeout=160)
            print("PASS " + deployment + " rollout", flush=True)
        apply(dict(apiVersion="v1", kind="Namespace", metadata=dict(name=namespace)))
        created = True
        apply(
            dict(
                apiVersion="v1", kind="Pod",
                metadata=dict(name="echo", namespace=namespace, labels=dict(app="echo")),
                spec=dict(restartPolicy="Never", containers=[dict(
                    name="echo", image=ALPINE,
                    command=["/bin/sh", "-ec",
                             "while :; do printf 'HTTP/1.0 200 OK\\r\\nContent-Length: 19\\r\\nConnection: close\\r\\n\\r\\nsapphire-network-ok' | nc -l -p 8080 >/dev/null; done"],
                    readinessProbe=dict(httpGet=dict(path="/", port=8080), periodSeconds=1),
                    resources=dict(limits=dict(memory="64Mi", cpu="100m")),
                )]),
            ),
            dict(
                apiVersion="v1", kind="Service", metadata=dict(name="echo", namespace=namespace),
                spec=dict(selector=dict(app="echo"), ports=[dict(port=80, targetPort=8080)]),
            ),
        )
        kube("-n", namespace, "wait", "--for=condition=Ready", "pod/echo", "--timeout=90s", timeout=100)
        url = f"http://echo.{namespace}.svc.cluster.local"
        client("dns-service", f"""
nslookup kubernetes.default.svc.cluster.local
nslookup echo.{namespace}.svc.cluster.local
nc -z -w 5 kubernetes.default.svc.cluster.local 443
test "$(wget -q -T 10 -O - {url})" = sapphire-network-ok
printf 'PASS pod DNS, Kubernetes API ClusterIP and HTTP service\\n'
""")
        apply(dict(
            apiVersion="networking.k8s.io/v1", kind="NetworkPolicy",
            metadata=dict(name="deny-echo", namespace=namespace),
            spec=dict(podSelector=dict(matchLabels=dict(app="echo")), policyTypes=["Ingress"], ingress=[]),
        ))
        client("deny-policy", f"""
for attempt in $(seq 1 20); do
    if ! wget -q -T 2 -O /dev/null {url}; then
        printf 'PASS ingress network policy denies service traffic\\n'
        exit 0
    fi
    sleep 1
done
exit 1
""")
        apply(dict(
            apiVersion="networking.k8s.io/v1", kind="NetworkPolicy",
            metadata=dict(name="allow-client", namespace=namespace),
            spec=dict(
                podSelector=dict(matchLabels=dict(app="echo")), policyTypes=["Ingress"],
                ingress=[{"from": [dict(podSelector=dict(matchLabels=dict(role="client")))],
                          "ports": [dict(protocol="TCP", port=8080)]}],
            ),
        ))
        client("allow-policy", f"""
for attempt in $(seq 1 20); do
    if test "$(wget -q -T 2 -O - {url})" = sapphire-network-ok; then
        printf 'PASS ingress network policy permits selected client\\n'
        exit 0
    fi
    sleep 1
done
exit 1
""")
        network_diagnostics()
        kube("delete", "namespace", namespace, "--wait=false", timeout=15)
        created = False
        ordinary_created = True
        docker("run", "-d", "--name", ordinary, ALPINE, "sleep", "600")
        call("k8s.stop")
        if docker("ps", "-a", "--filter", "label=io.kubernetes.pod.uid", "--format", "{{.ID}}"):
            raise RuntimeError("k8s.stop left Kubernetes pod containers or sandboxes")
        if docker("inspect", "--format", "{{.State.Running}}", ordinary) != "true":
            raise RuntimeError("k8s.stop stopped an ordinary Docker container")
        docker(
            "run", "--rm", "--pid=host", "--network=host", "--cap-add", "SYS_PTRACE",
            ALPINE, "chroot", "/proc/1/root", "/bin/sh", "-ec",
            "grep -qxF 'populated 0' /sys/fs/cgroup/sapphire/services/k3s/cgroup.events",
        )
        docker("rm", "--force", ordinary)
        ordinary_created = False
        print("PASS k8s.stop removes pod workloads and helpers while preserving ordinary Docker", flush=True)
        passed = True
    finally:
        if ordinary_created:
            try:
                docker("rm", "--force", ordinary)
            except Exception as error:
                print(f"ordinary container cleanup: {error}", file=sys.stderr)
        if machine_created:
            for action in ("machine.stop", "machine.delete"):
                try:
                    call(action, name=machine)
                except Exception as error:
                    print(f"machine cleanup: {error}", file=sys.stderr)
        if created:
            try:
                kube("delete", "namespace", namespace, "--wait=false", timeout=15)
            except Exception as error:
                print(f"namespace cleanup: {error}", file=sys.stderr)
        if not passed and kubeconfig.exists():
            try:
                print(kube("get", "pods", "-A", "-o", "wide", timeout=15), file=sys.stderr)
            except Exception as error:
                print(f"failure diagnostics: {error}", file=sys.stderr)
        try:
            engine("stop", "--json", timeout=65)
        finally:
            kubeconfig.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
