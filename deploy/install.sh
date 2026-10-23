#!/usr/bin/env bash
# Nube installer for Ubuntu (20.04+).
#
#   sudo bash install.sh single                       # server + agent on this machine
#   sudo bash install.sh server                       # control plane only
#   sudo bash install.sh server --ha --id s1 --bootstrap
#   sudo bash install.sh server --ha --id s2 --join https://10.0.0.1:7070 --token ADMIN_TOKEN --ca-hash sha256:...
#   sudo bash install.sh agent --server https://10.0.0.1:7070 --token NODE_TOKEN --ca-hash sha256:...
#   sudo bash install.sh uninstall
#
# Common options:
#   --binary PATH      nube binary to install (default: next to this script)
#   --ip ADDR          this machine's IP as seen by other machines (default: auto)
#   --network MODE     pod overlay for agents: wireguard (default) | host-gw | none
#   --name NAME        node name (default: hostname)
#   --labels k=v,...   node labels
#   --no-tls           serve the API over plain HTTP (not recommended)
#   --no-ingress       do not listen on ports 80/443 for Ingress
#   --no-packages      do not apt-get install dependencies
set -euo pipefail

ROLE="${1:-}"; shift || true
BINARY="" IP="" TOKEN="" SERVER="" NETWORK="wireguard" NAME="" LABELS="" CA_HASH=""
HA=false ID="" BOOTSTRAP=false JOIN="" PACKAGES=true TLS=true INGRESS=true

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --ip) IP="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --server) SERVER="$2"; shift 2 ;;
    --ca-hash) CA_HASH="$2"; shift 2 ;;
    --network) NETWORK="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --labels) LABELS="$2"; shift 2 ;;
    --ha) HA=true; shift ;;
    --id) ID="$2"; shift 2 ;;
    --bootstrap) BOOTSTRAP=true; shift ;;
    --join) JOIN="$2"; shift 2 ;;
    --no-tls) TLS=false; shift ;;
    --no-ingress) INGRESS=false; shift ;;
    --no-packages) PACKAGES=false; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0 ...)"
case "$ROLE" in single|server|agent|uninstall) ;; *) sed -n '2,22p' "$0"; exit 2 ;; esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCHEME=https
if ! $TLS; then SCHEME=http; fi

if [[ "$ROLE" == uninstall ]]; then
  say "stopping services"
  systemctl disable --now nube-agent.service nube-server.service 2>/dev/null || true
  rm -f /etc/systemd/system/nube-agent.service /etc/systemd/system/nube-server.service /usr/local/bin/nube
  systemctl daemon-reload
  ip link del nube-wg 2>/dev/null || true
  say "removed binary and services. Kept data and config:"
  echo "    /etc/nube  /var/lib/nube  /var/lib/nube-agent"
  echo "  Containers started by Nube keep running; list them with: docker ps --filter label=nube.managed=true"
  exit 0
fi

# ---------------------------------------------------------------- binary
ARCH="$(uname -m)"
case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "unsupported architecture $ARCH" ;; esac
if [[ -z "$BINARY" ]]; then
  for c in "$SCRIPT_DIR/nube-linux-$ARCH" "$SCRIPT_DIR/../dist/nube-linux-$ARCH" "$SCRIPT_DIR/nube" "./nube-linux-$ARCH" "./nube"; do
    if [[ -f "$c" ]]; then BINARY="$c"; break; fi
  done
fi
[[ -n "$BINARY" && -f "$BINARY" ]] || die "nube binary not found; pass --binary /path/to/nube-linux-$ARCH"
install -m 0755 "$BINARY" /usr/local/bin/nube
say "installed /usr/local/bin/nube"

# ---------------------------------------------------------------- packages
need_agent=false
if [[ "$ROLE" == agent || "$ROLE" == single ]]; then need_agent=true; fi
if $PACKAGES && $need_agent; then
  say "installing dependencies (docker, wireguard, iptables, nfs, e2fsprogs)"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  pkgs=(iptables iproute2 nfs-common ca-certificates curl e2fsprogs util-linux mount)
  if [[ "$NETWORK" == wireguard ]]; then pkgs+=(wireguard-tools); fi
  if ! command -v docker >/dev/null; then pkgs+=(docker.io); fi
  apt-get install -y -qq "${pkgs[@]}" >/dev/null
  systemctl enable --now docker >/dev/null
fi
if $need_agent; then
  command -v docker >/dev/null || die "docker is required on agents (apt install docker.io)"
  if [[ "$NETWORK" == wireguard ]]; then
    # Built-in or loadable, the reliable test is creating an interface.
    modprobe wireguard 2>/dev/null || true
    if ! command -v wg >/dev/null || ! { ip link add nube-wgtest type wireguard 2>/dev/null && ip link del nube-wgtest; }; then
      warn "WireGuard unavailable (kernel module or wireguard-tools missing); using --network host-gw"
      NETWORK=host-gw
    fi
  fi
fi

# ---------------------------------------------------------------- addresses & token
if [[ -z "$IP" ]]; then
  IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
  [[ -n "$IP" ]] || IP="$(hostname -I | awk '{print $1}')"
fi
if [[ -z "$NAME" ]]; then NAME="$(hostname | tr 'A-Z_' 'a-z-')"; fi
if [[ -z "$ID" ]]; then ID="$NAME"; fi
mkdir -p /etc/nube && chmod 700 /etc/nube

ADMIN_TOKEN=""
if [[ "$ROLE" == single || "$ROLE" == server ]]; then
  mkdir -p /var/lib/nube && chmod 700 /var/lib/nube
  ADMIN_TOKEN="$TOKEN"
  if [[ -z "$ADMIN_TOKEN" && -s /var/lib/nube/token ]]; then ADMIN_TOKEN="$(cat /var/lib/nube/token)"; fi
  if [[ -z "$ADMIN_TOKEN" ]]; then
    if [[ -n "$JOIN" ]]; then die "--join needs the admin --token of the existing servers"; fi
    ADMIN_TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  fi
  (umask 077; echo "$ADMIN_TOKEN" > /var/lib/nube/token)
  if [[ -n "$JOIN" && "$JOIN" == https://* && -z "$CA_HASH" ]]; then
    die "--join over https needs --ca-hash (see /var/lib/nube/ca.hash on the first server)"
  fi
fi
if [[ "$ROLE" == agent ]]; then
  [[ -n "$TOKEN" ]] || die "--token is required (on a server: nube token create NAME --role node)"
  [[ -n "$SERVER" ]] || die "--server is required, e.g. --server https://10.0.0.1:7070"
  if [[ "$SERVER" == https://* && -z "$CA_HASH" ]]; then die "--ca-hash is required (see /var/lib/nube/ca.hash on a server)"; fi
fi

# ---------------------------------------------------------------- server
if [[ "$ROLE" == single || "$ROLE" == server ]]; then
  say "configuring control plane (API over $SCHEME)"
  {
    echo "NUBE_TOKEN=$ADMIN_TOKEN"
    echo "NUBE_LISTEN=:7070"
    echo "NUBE_DATA=/var/lib/nube"
    echo "NUBE_TLS=$TLS"
    echo "NUBE_TLS_SAN=$IP"
    if ! $INGRESS; then echo "NUBE_INGRESS_HTTP=off"; echo "NUBE_INGRESS_HTTPS=off"; fi
    if $HA; then
      echo "NUBE_HA=true"
      echo "NUBE_SERVER_ID=$ID"
      echo "NUBE_RAFT_ADVERTISE=$IP:7072"
      echo "NUBE_ADVERTISE_API=$SCHEME://$IP:7070"
      if $BOOTSTRAP; then echo "NUBE_BOOTSTRAP=true"; fi
      if [[ -n "$JOIN" ]]; then echo "NUBE_JOIN=$JOIN"; fi
      if [[ -n "$CA_HASH" ]]; then echo "NUBE_CA_HASH=$CA_HASH"; fi
    fi
  } > /etc/nube/server.env
  chmod 600 /etc/nube/server.env
  install -m 0644 "$SCRIPT_DIR/systemd/nube-server.service" /etc/systemd/system/nube-server.service 2>/dev/null ||
    cat > /etc/systemd/system/nube-server.service <<'UNIT'
[Unit]
Description=Nube control plane
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/nube/server.env
ExecStart=/usr/local/bin/nube server
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable nube-server.service >/dev/null
  systemctl restart nube-server.service
  for _ in $(seq 1 30); do curl -skf "$SCHEME://127.0.0.1:7070/healthz" >/dev/null && break; sleep 1; done
  curl -skf "$SCHEME://127.0.0.1:7070/healthz" >/dev/null || die "server did not start; see: journalctl -u nube-server -n 50"
  if $TLS; then
    # The leader creates the cluster CA; joining servers receive it through Raft.
    for _ in $(seq 1 60); do [[ -s /var/lib/nube/ca.hash ]] && break; sleep 1; done
    [[ -s /var/lib/nube/ca.hash ]] || die "no cluster CA yet; is a leader elected? (journalctl -u nube-server)"
    CA_HASH="$(tr -d ' \n' < /var/lib/nube/ca.hash)"
  fi
  if [[ -z "$SERVER" ]]; then SERVER="$SCHEME://127.0.0.1:7070"; fi
fi

# A node token for agents (the admin token never leaves the servers).
NODE_TOKEN=""
if [[ "$ROLE" == single || ( "$ROLE" == server && ! -n "$JOIN" ) ]]; then
  for _ in $(seq 1 20); do
    NODE_TOKEN="$(NUBE_SERVER="$SCHEME://127.0.0.1:7070" NUBE_TOKEN="$ADMIN_TOKEN" NUBE_CA_HASH="$CA_HASH" NUBE_CONFIG=/dev/null \
      /usr/local/bin/nube token create "nodes-$(head -c 3 /dev/urandom | od -An -tx1 | tr -d ' \n')" --role node 2>/dev/null |
      grep -o 'nube_[A-Za-z0-9_]*' || true)"
    [[ -n "$NODE_TOKEN" ]] && break
    sleep 1
  done
  [[ -n "$NODE_TOKEN" ]] || warn "could not create a node token; create one with: nube token create nodes --role node"
fi

# ---------------------------------------------------------------- agent
if $need_agent; then
  AGENT_TOKEN="$TOKEN"
  if [[ "$ROLE" == single ]]; then AGENT_TOKEN="$NODE_TOKEN"; fi
  [[ -n "$AGENT_TOKEN" ]] || die "no token for the agent"
  say "configuring node agent ($NETWORK network)"
  {
    echo "NUBE_SERVER=$SERVER"
    echo "NUBE_TOKEN=$AGENT_TOKEN"
    if [[ -n "$CA_HASH" ]]; then echo "NUBE_CA_HASH=$CA_HASH"; fi
    echo "NUBE_NODE_NAME=$NAME"
    echo "NUBE_ADVERTISE_IP=$IP"
    echo "NUBE_NETWORK=$NETWORK"
    if [[ -n "$LABELS" ]]; then echo "NUBE_LABELS=$LABELS"; fi
  } > /etc/nube/agent.env
  chmod 600 /etc/nube/agent.env
  install -m 0644 "$SCRIPT_DIR/systemd/nube-agent.service" /etc/systemd/system/nube-agent.service 2>/dev/null ||
    cat > /etc/systemd/system/nube-agent.service <<'UNIT'
[Unit]
Description=Nube node agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
EnvironmentFile=/etc/nube/agent.env
ExecStart=/usr/local/bin/nube agent
Restart=always
RestartSec=3
KillMode=process
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable nube-agent.service >/dev/null
  systemctl restart nube-agent.service
fi

# ---------------------------------------------------------------- client config
if [[ -n "${SUDO_USER:-}" && "$ROLE" != agent ]]; then
  home="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
  mkdir -p "$home/.nube"
  printf '{\n  "server": "%s://127.0.0.1:7070",\n  "token": "%s",\n  "caHash": "%s"\n}\n' "$SCHEME" "$ADMIN_TOKEN" "$CA_HASH" > "$home/.nube/config.json"
  chown -R "$SUDO_USER": "$home/.nube"; chmod 600 "$home/.nube/config.json"
fi

# ---------------------------------------------------------------- summary
echo
say "done"
if [[ "$ROLE" != agent ]]; then
  echo "  Dashboard:   $SCHEME://$IP:7070/   (sign in with the admin token)"
  echo "  Admin token: in /var/lib/nube/token"
  if [[ -n "$CA_HASH" ]]; then
    echo "  Cluster CA:  $CA_HASH"
    echo "               download $SCHEME://$IP:7070/ca.crt and import it in your browser to avoid the warning"
  fi
  echo "  Try:         nube get nodes && nube apply -f examples/web.yaml"
  if [[ -n "$NODE_TOKEN" ]]; then
    echo
    echo "  Add a worker node (run on the other machine):"
    echo "    sudo bash install.sh agent --server $SCHEME://$IP:7070 --token $NODE_TOKEN${CA_HASH:+ --ca-hash $CA_HASH}"
  fi
  if $HA && $BOOTSTRAP; then
    echo
    echo "  Add control-plane servers (run on each of them):"
    echo "    sudo bash install.sh server --ha --id s2 --join $SCHEME://$IP:7070 --token \$(cat /var/lib/nube/token)${CA_HASH:+ --ca-hash $CA_HASH}"
    echo "    (copy the admin token from this machine's /var/lib/nube/token)"
  fi
fi
if [[ "$ROLE" == agent ]]; then
  echo "  This node will appear in 'nube get nodes' within a few seconds."
fi
echo
echo "  Firewall: allow 7070/tcp (API), 7071/tcp (agent), 7072/tcp (HA raft), 51820/udp (WireGuard)"
echo "            between machines; 80,443/tcp (Ingress) and your service ports for clients."
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  echo "  ufw is active, e.g.: sudo ufw allow 80,443/tcp && sudo ufw allow from 10.0.0.0/8 to any port 7070:7072 proto tcp && sudo ufw allow 51820/udp"
fi
echo "  Logs:     journalctl -u nube-server -f    |    journalctl -u nube-agent -f"
