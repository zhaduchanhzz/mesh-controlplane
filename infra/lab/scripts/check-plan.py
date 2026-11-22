#!/usr/bin/env python3
"""Gate for agent self-apply (AGENTS.md rule 3, 04-proxmox-lab.md §6).

    tofu show -json lab.tfplan | python3 scripts/check-plan.py

Exit 0: the plan only creates/updates resources inside the lab scope, so the
agent may apply this exact plan file. Exit 2: human approval required.

SDN resources (zones, VNets, subnets, appliers, ...) are host network changes
(04-proxmox-lab.md §5): any create/update/delete of them always needs a human.
"""

import json
import sys

LAB_POOL = "meshcontrol-lab"
VMID_MIN, VMID_MAX = 7700, 7799
SDN_PREFIXES = ("proxmox_sdn_", "proxmox_virtual_environment_sdn_")


def check(rc: dict) -> str | None:
    """Return a reason the change needs approval, or None if in scope."""
    actions = rc["change"]["actions"]
    addr = rc["address"]
    if actions in (["no-op"], ["read"]):
        return None
    if rc["type"].startswith(SDN_PREFIXES):
        return f"{addr}: {'/'.join(actions)}: SDN is a host network change, human approval required"
    if "delete" in actions:
        return f"{addr}: {'/'.join(actions)} (destroy or replace)"

    after = rc["change"].get("after") or {}
    typ = rc["type"]
    if typ == "proxmox_virtual_environment_vm":
        vmid = after.get("vm_id")
        if after.get("pool_id") != LAB_POOL:
            return f"{addr}: pool_id={after.get('pool_id')!r}, want {LAB_POOL!r}"
        if not isinstance(vmid, int) or not VMID_MIN <= vmid <= VMID_MAX:
            return f"{addr}: vm_id={vmid!r} outside {VMID_MIN}-{VMID_MAX}"
        return None
    if typ in ("proxmox_download_file", "proxmox_virtual_environment_download_file"):
        ok = (
            after.get("datastore_id") == "local"
            and after.get("content_type") == "import"
            and str(after.get("file_name", "")).startswith("mc-")
            and after.get("checksum")
        )
        return None if ok else f"{addr}: download must be a checksummed mc-* file in local:import"
    if typ == "proxmox_virtual_environment_file":
        raw = (after.get("source_raw") or [{}])[0]
        ok = (
            after.get("datastore_id") == "local"
            and after.get("content_type") == "snippets"
            and str(raw.get("file_name", "")).startswith("mc-")
        )
        return None if ok else f"{addr}: file must be an mc-* snippet in local:snippets"
    return f"{addr}: resource type {typ} is outside the self-apply scope"


def main() -> int:
    plan = json.load(sys.stdin)
    changes = plan.get("resource_changes", [])
    problems = [p for p in (check(rc) for rc in changes) if p]
    for rc in changes:
        print(f"  {'/'.join(rc['change']['actions']):<14} {rc['address']}")
    if problems:
        print("\nHUMAN APPROVAL REQUIRED:")
        for p in problems:
            print(f"  - {p}")
        return 2
    print("\nIn lab scope: agent may apply this exact plan file.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
