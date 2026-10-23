#!/usr/bin/env bash
# Starts a local test cluster on Docker: 3 HA control-plane servers (TLS)
# and 2 worker nodes (Docker-in-Docker + WireGuard).
#
#   ./up.sh          start
#   ./up.sh nodes    recreate the two nodes (e.g. after rebuilding the image)
#   ./up.sh down     remove everything
#   SERVER_ARGS="--acme-email ..." ./up.sh   pass extra flags to the servers
#
# Then:  export NUBE_SERVER=https://127.0.0.1:7070 NUBE_TOKEN=testtoken123 NUBE_CA_HASH=$(cat .ca-hash)
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")"
T=testtoken123

if [[ "${1:-}" == down ]]; then
  docker rm -f nube-s1 nube-s2 nube-s3 nube-n1 nube-n2 >/dev/null 2>&1 || true
  docker network rm nubetest >/dev/null 2>&1 || true
  exit 0
fi

start_nodes() {
  local CA NODE_TOKEN IMG
  CA=$(cat .ca-hash)
  NODE_TOKEN=$(cat .node-token)
  IMG="$(pwd -W 2>/dev/null || pwd)/images"
  for i in 1 2; do
    docker rm -f "nube-n$i" >/dev/null 2>&1 || true
    # --tmpfs /run: Docker's runtime pid files must not survive a container restart.
    docker run -d --privileged --tmpfs /run --name "nube-n$i" --hostname "node$i" --network nubetest --ip "172.30.0.2$i"       -v "$IMG":/images:ro nube-test-node       --server https://172.30.0.11:7070,https://172.30.0.12:7070,https://172.30.0.13:7070       --token "$NODE_TOKEN" --ca-hash "$CA" --name "node$i" --advertise-ip "172.30.0.2$i" --network wireguard >/dev/null
  done
}

if [[ "${1:-}" == nodes ]]; then
  start_nodes
  echo "nodes recreated"
  exit 0
fi

docker network inspect nubetest >/dev/null 2>&1 || docker network create nubetest --subnet 172.30.0.0/24 >/dev/null

# server ID IP [docker args...] -- [nube args...]
server() {
  local id=$1 ip=$2; shift 2
  local dargs=() nargs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do dargs+=("$1"); shift; done
  [[ $# -gt 0 ]] && shift
  nargs=("$@")
  docker run -d --name "nube-$id" --network nubetest --ip "$ip" "${dargs[@]}" nube-test-server \
    --ha --id "$id" --token "$T" --data /data --raft-advertise "$ip:7072" --advertise-api "https://$ip:7070" \
    "${nargs[@]}" ${SERVER_ARGS:-} >/dev/null
}

server s1 172.30.0.11 -p 7070:7070 -p 18080:8080 -p 18081:80 -p 18443:443 -- --bootstrap

echo -n "waiting for the cluster CA"
for _ in $(seq 1 30); do
  docker exec nube-s1 cat /data/ca.hash >/dev/null 2>&1 && break
  echo -n .
  sleep 1
done
echo
CA=$(docker exec nube-s1 cat /data/ca.hash | tr -d '\r\n')
echo "$CA" > .ca-hash
echo "CA: $CA"

server s2 172.30.0.12 -p 7071:7070 -- --join https://172.30.0.11:7070 --ca-hash "$CA"
server s3 172.30.0.13 -- --join https://172.30.0.11:7070 --ca-hash "$CA"

# A node token (role "node") for the agents; the admin token never leaves the servers.
NODE_TOKEN=$(curl -sk -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
  -d '{"name":"e2e-nodes","role":"node"}' https://127.0.0.1:7070/api/v1/tokens |
  sed -n 's/.*"secret": *"\([^"]*\)".*/\1/p')
[[ -n "$NODE_TOKEN" ]] || { echo "could not create a node token"; exit 1; }
echo "$NODE_TOKEN" > .node-token

start_nodes
echo "cluster starting: servers s1 (:7070) s2 (:7071) s3, nodes node1 node2"
