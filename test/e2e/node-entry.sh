#!/bin/sh
# Start an inner dockerd, wait for it, preload test images, then run the agent.
dockerd-entrypoint.sh dockerd >/var/log/dockerd.log 2>&1 &
for i in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
for f in /images/*.tar; do [ -f "$f" ] && docker load -i "$f" >/dev/null; done
exec nube agent "$@"
