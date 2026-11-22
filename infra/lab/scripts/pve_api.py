#!/usr/bin/env python3
"""Minimal Proxmox VE API client for the mesh-controlplane lab.

- Verifies TLS against a pinned PVE certificate (never an insecure mode).
- Connects to PVE_IP while checking the certificate for PVE_HOST, because the
  node certificate's SAN may not list the host's current IP.
- Reads credentials from an env file outside the repo
  (default: ~/.mesh-lab/proxmox.env) and never prints secrets or tickets.
- Read-only unless --write is given.

Usage:
  python infra/lab/scripts/pve_api.py inventory
  python infra/lab/scripts/pve_api.py get /nodes/<node>/status
  python infra/lab/scripts/pve_api.py --write post /pools poolid=meshcontrol-lab
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
import socket
import ssl
import sys
import urllib.parse
from pathlib import Path

LAB_VMID_MIN, LAB_VMID_MAX = 7700, 7799
LAB_POOL = "meshcontrol-lab"


def load_env(path: Path) -> dict[str, str]:
    env: dict[str, str] = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
            value = value[1:-1]
        env[key.strip()] = value
    return env


def native_path(p: str) -> str:
    """Turn a Git-Bash path (/c/Users/...) into C:/Users/... on Windows."""
    if os.name == "nt" and len(p) > 3 and p[0] == "/" and p[2] == "/":
        return f"{p[1].upper()}:{p[2:]}"
    return p


class PVE:
    def __init__(self, env: dict[str, str]) -> None:
        self.host = env["PVE_HOST"]
        self.ip = env["PVE_IP"]
        ctx = ssl.create_default_context(cafile=native_path(env["PVE_CERT"]))
        # The pinned file is the node's leaf certificate; trust it as an anchor.
        ctx.verify_flags |= ssl.VERIFY_X509_PARTIAL_CHAIN
        # PVE's self-generated certificate is not RFC 5280-strict.
        ctx.verify_flags &= ~getattr(ssl, "VERIFY_X509_STRICT", 0)
        self.ctx = ctx
        self.ticket: str | None = None
        self.csrf: str | None = None
        data = self.request(
            "POST",
            "/access/ticket",
            {"username": env["PROXMOX_VE_USERNAME"], "password": env["PROXMOX_VE_PASSWORD"]},
        )
        self.ticket, self.csrf = data["ticket"], data["CSRFPreventionToken"]

    def _connection(self) -> http.client.HTTPSConnection:
        sock = socket.create_connection((self.ip, 8006), timeout=30)
        conn = http.client.HTTPSConnection(self.host, 8006, context=self.ctx, timeout=30)
        conn.sock = self.ctx.wrap_socket(sock, server_hostname=self.host)
        return conn

    def request(self, method: str, path: str, params: dict[str, str] | None = None):
        params = params or {}
        body = None
        if method == "GET":
            if params:
                path = f"{path}?{urllib.parse.urlencode(params)}"
        else:
            body = urllib.parse.urlencode(params)
        headers = {"Content-Type": "application/x-www-form-urlencoded"}
        if self.ticket:
            headers["Cookie"] = f"PVEAuthCookie={self.ticket}"
            if method != "GET":
                headers["CSRFPreventionToken"] = self.csrf or ""
        conn = self._connection()
        try:
            conn.request(method, f"/api2/json{path}", body=body, headers=headers)
            resp = conn.getresponse()
            raw = resp.read()
        finally:
            conn.close()
        if resp.status >= 400:
            # Never echo the request body: it may hold the password.
            detail = raw.decode("utf-8", "replace")[:300] if path != "/access/ticket" else ""
            raise SystemExit(f"HTTP {resp.status} {resp.reason} for {method} {path} {detail}".rstrip())
        return json.loads(raw or b"{}").get("data")


def gib(n) -> str:
    return f"{(n or 0) / 1024**3:.1f} GiB"


def inventory(pve: PVE) -> None:
    ver = pve.request("GET", "/version")
    print(f"PVE version: {ver.get('version')} (release {ver.get('release')})")

    for node in pve.request("GET", "/nodes"):
        name = node["node"]
        st = pve.request("GET", f"/nodes/{name}/status")
        cpu = st.get("cpuinfo", {})
        mem = st.get("memory", {})
        print(f"\nNode {name}: {node.get('status')}")
        print(f"  CPU: {cpu.get('model')} | {cpu.get('cpus')} threads ({cpu.get('sockets')} socket)")
        print(f"  RAM: total {gib(mem.get('total'))}, used {gib(mem.get('used'))}, free {gib(mem.get('free'))}")
        print(f"  Root FS: total {gib(st.get('rootfs', {}).get('total'))}, avail {gib(st.get('rootfs', {}).get('avail'))}")

        print("  Storage:")
        for s in pve.request("GET", f"/nodes/{name}/storage"):
            print(
                f"    - {s['storage']:<14} type={s.get('type'):<8} content={s.get('content')} "
                f"active={s.get('active')} avail={gib(s.get('avail'))} total={gib(s.get('total'))}"
            )

        print("  Network:")
        for i in sorted(pve.request("GET", f"/nodes/{name}/network"), key=lambda x: x.get("iface", "")):
            if i.get("type") in ("bridge", "OVSBridge", "eth", "bond", "vlan"):
                print(
                    f"    - {i['iface']:<10} type={i.get('type'):<7} cidr={i.get('cidr') or '-'} "
                    f"gw={i.get('gateway') or '-'} ports={i.get('bridge_ports') or '-'} "
                    f"vlan_aware={i.get('bridge_vlan_aware') or 0} active={i.get('active')}"
                )

    pools = [p["poolid"] for p in pve.request("GET", "/pools")]
    print(f"\nPools: {pools or '[]'} (lab pool exists: {LAB_POOL in pools})")

    vms = pve.request("GET", "/cluster/resources", {"type": "vm"})
    print(f"\nVMs/CTs on host: {len(vms)}")
    for v in sorted(vms, key=lambda x: x["vmid"]):
        print(f"    - {v['vmid']:>5} {v.get('type'):<4} {v.get('name', ''):<28} {v.get('status'):<8} pool={v.get('pool', '-')}")
    taken = [v["vmid"] for v in vms if LAB_VMID_MIN <= v["vmid"] <= LAB_VMID_MAX]
    print(f"\nLab VMID range {LAB_VMID_MIN}-{LAB_VMID_MAX}: {'FREE' if not taken else f'IN USE {taken}'}")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--env", default=str(Path.home() / ".mesh-lab" / "proxmox.env"))
    ap.add_argument("--write", action="store_true", help="allow POST/PUT/DELETE")
    ap.add_argument("command", choices=["inventory", "get", "post", "put", "delete"])
    ap.add_argument("path", nargs="?")
    ap.add_argument("params", nargs="*", help="key=value")
    args = ap.parse_args()

    method = "GET" if args.command in ("inventory", "get") else args.command.upper()
    if method != "GET" and not args.write:
        sys.exit("refusing to mutate without --write")

    pve = PVE(load_env(Path(args.env)))
    if args.command == "inventory":
        inventory(pve)
        return
    if not args.path:
        sys.exit("path required")
    if ":" in args.path.split("/", 1)[0]:
        sys.exit("path looks rewritten by Git Bash (MSYS); pass it without the leading '/' or set MSYS_NO_PATHCONV=1")
    if not args.path.startswith("/"):
        args.path = "/" + args.path
    params = dict(p.split("=", 1) for p in args.params)
    print(json.dumps(pve.request(method, args.path, params), indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
