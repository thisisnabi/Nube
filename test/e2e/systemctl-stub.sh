#!/bin/bash
# Test-only stand-in for systemctl inside a container without systemd:
# "start/restart X" runs the unit's ExecStart with its EnvironmentFile.
args=()
for a in "$@"; do [[ "$a" == --* ]] || args+=("$a"); done
cmd="${args[0]}"
for unit in "${args[@]:1}"; do
  unit="${unit%.service}"
  if [[ "$unit" == docker ]]; then
    if [[ "$cmd" == start || "$cmd" == restart || "$*" == *--now* ]]; then
      pgrep -x dockerd >/dev/null || (dockerd >/var/log/dockerd.log 2>&1 &)
      for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
    fi
    continue
  fi
  file="/etc/systemd/system/$unit.service"
  [[ -f "$file" ]] || continue
  case "$cmd" in
    start|restart)
      pkill -f "nube ${unit#nube-}" 2>/dev/null; sleep 1
      envfile="$(sed -n 's/^EnvironmentFile=//p' "$file")"
      exec_start="$(sed -n 's/^ExecStart=//p' "$file")"
      (set -a; . "$envfile"; set +a; nohup $exec_start >"/var/log/$unit.log" 2>&1 &) ;;
  esac
done
exit 0
