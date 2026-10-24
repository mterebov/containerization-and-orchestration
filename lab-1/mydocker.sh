#!/usr/bin/env bash
# mydocker.sh: api в своих namespaces, под лимитами cgroup, без capabilities и под seccomp.
set -euo pipefail

CG=/sys/fs/cgroup/mydocker
API=$HOME/api
PROFILE=$HOME/seccomp.json
LOADER=$HOME/seccomp_exec.py

# 1. cgroup и лимиты. Единственное место, где нужен root: /sys/fs/cgroup принадлежит хосту.
sudo mkdir -p "$CG"
echo "50000 100000" | sudo tee "$CG/cpu.max" >/dev/null
echo 20 | sudo tee "$CG/pids.max" >/dev/null
echo 64M | sudo tee "$CG/memory.max" >/dev/null
echo 0 | sudo tee "$CG/memory.swap.max" >/dev/null

# 2. Переезд в cgroup до запуска: переносим сам скрипт, всё, что он породит, унаследует группу.
echo $$ | sudo tee "$CG/cgroup.procs" >/dev/null

# 3. namespaces -> lo up -> seccomp -> capabilities -> api.
# exec не меняет PID: sh становится python3, тот setpriv, тот api, и api остаётся PID 1.
exec unshare --user --map-root-user --pid --fork --mount --mount-proc --net --uts --ipc \
  sh -c "ip link set lo up && exec python3 $LOADER $PROFILE -- setpriv --bounding-set=-all --inh-caps=-all $API"
