#!/usr/bin/env python3
"""Apply a Docker-style seccomp profile to this process, then exec the command.

Usage: seccomp_exec.py PROFILE.json -- CMD [ARGS...]

Understands the subset of Docker's profile format that this lab needs:
  "defaultAction": SCMP_ACT_ALLOW | SCMP_ACT_ERRNO | SCMP_ACT_KILL | SCMP_ACT_LOG
  "syscalls": [{"names": [...], "action": "...", "errnoRet": N}]
Argument conditions, architectures and includes/excludes are ignored.
Needs the python3-seccomp package (libseccomp bindings).

The filter is loaded right before exec, so the command and every child it
forks run under it; a loaded filter cannot be removed by the process itself.
"""
import json
import os
import sys

import seccomp

ACTIONS = {
    "SCMP_ACT_ALLOW": lambda rule: seccomp.ALLOW,
    "SCMP_ACT_ERRNO": lambda rule: seccomp.ERRNO(rule.get("errnoRet", 1)),
    "SCMP_ACT_KILL": lambda rule: seccomp.KILL,
    "SCMP_ACT_KILL_PROCESS": lambda rule: seccomp.KILL_PROCESS,
    "SCMP_ACT_LOG": lambda rule: seccomp.LOG,
}


def main():
    if len(sys.argv) < 4 or sys.argv[2] != "--":
        sys.exit(__doc__)
    with open(sys.argv[1]) as fh:
        profile = json.load(fh)
    cmd = sys.argv[3:]

    flt = seccomp.SyscallFilter(ACTIONS[profile["defaultAction"]](profile))
    rules = 0
    for rule in profile.get("syscalls", []):
        action = ACTIONS[rule["action"]](rule)
        for name in rule["names"]:
            try:
                flt.add_rule(action, name)
                rules += 1
            except Exception:
                # syscall name unknown on this architecture: skip it
                pass
    # load() installs the BPF program via seccomp(2); libseccomp also sets
    # no_new_privs first, so this works without any capability.
    flt.load()
    print(f"seccomp: {rules} rules loaded from {sys.argv[1]}, exec {cmd[0]}",
          file=sys.stderr)
    os.execvp(cmd[0], cmd)


if __name__ == "__main__":
    main()
