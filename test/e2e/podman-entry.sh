#!/bin/sh
# Start Podman's Docker-compatible API, preload test images, run the agent.
mkdir -p /run/podman
podman system service --time=0 unix:///run/podman/podman.sock >/var/log/podman.log 2>&1 &
for i in $(seq 1 30); do [ -S /run/podman/podman.sock ] && break; sleep 1; done
for f in /images/*.tar; do [ -f "$f" ] && podman load -q -i "$f" >/dev/null 2>&1; done
exec nube agent --runtime podman "$@"
