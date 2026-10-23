// Command nube is a single binary for the control plane, the node agent and the CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	_ "time/tzdata" // CronJob time zones work even on hosts without tzdata

	"github.com/thisisnabi/Nube/internal/agent"
	"github.com/thisisnabi/Nube/internal/cli"
	"github.com/thisisnabi/Nube/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "server":
			os.Exit(runServer(os.Args[2:]))
		case "agent":
			os.Exit(runAgent(os.Args[2:]))
		case "pause":
			// Entry point of a pod's pause container: hold the network namespace until stopped.
			ctx, cancel := signalContext()
			defer cancel()
			<-ctx.Done()
			os.Exit(0)
		}
	}
	os.Exit(cli.Run(os.Args[1:]))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	var cfg server.Config
	fs.StringVar(&cfg.Listen, "listen", envOr("NUBE_LISTEN", ":7070"), "API and dashboard listen address")
	fs.StringVar(&cfg.DataDir, "data", envOr("NUBE_DATA", "/var/lib/nube"), "data directory")
	fs.StringVar(&cfg.Token, "token", os.Getenv("NUBE_TOKEN"), "cluster token (generated into <data>/token if empty)")
	fs.StringVar(&cfg.ServiceBind, "service-bind", envOr("NUBE_SERVICE_BIND", "0.0.0.0"), "address service load-balancer ports bind to")
	fs.StringVar(&cfg.ClusterCIDR, "cluster-cidr", envOr("NUBE_CLUSTER_CIDR", "10.88.0.0/16"), "pod network (/16, one /24 per node)")
	fs.StringVar(&cfg.Domain, "domain", envOr("NUBE_DOMAIN", "nube"), "cluster DNS domain")
	fs.BoolVar(&cfg.HA, "ha", os.Getenv("NUBE_HA") == "true", "run as a member of a highly-available (Raft) control plane")
	fs.StringVar(&cfg.ServerID, "id", os.Getenv("NUBE_SERVER_ID"), "unique server id in HA mode (default: hostname)")
	fs.StringVar(&cfg.RaftBind, "raft-bind", envOr("NUBE_RAFT_BIND", "0.0.0.0:7072"), "Raft listen address")
	fs.StringVar(&cfg.RaftAdvertise, "raft-advertise", os.Getenv("NUBE_RAFT_ADVERTISE"), "Raft address other servers use, e.g. 10.0.0.1:7072")
	fs.StringVar(&cfg.APIAdvertise, "advertise-api", os.Getenv("NUBE_ADVERTISE_API"), "API URL other servers use, e.g. https://10.0.0.1:7070")
	fs.BoolVar(&cfg.Bootstrap, "bootstrap", os.Getenv("NUBE_BOOTSTRAP") == "true", "initialize a new HA cluster with this server (first server only)")
	fs.StringVar(&cfg.Join, "join", os.Getenv("NUBE_JOIN"), "API URL of an existing HA server to join")
	fs.BoolVar(&cfg.TLS, "tls", os.Getenv("NUBE_TLS") != "false", "serve the API over HTTPS with the cluster CA")
	sans := fs.String("tls-san", os.Getenv("NUBE_TLS_SAN"), "extra names/IPs for the server certificate, comma separated")
	fs.StringVar(&cfg.CAHash, "ca-hash", os.Getenv("NUBE_CA_HASH"), "cluster CA to trust when joining (HA), sha256:...")
	fs.StringVar(&cfg.IngressHTTP, "ingress-http", envOr("NUBE_INGRESS_HTTP", ":80"), "ingress HTTP address (\"off\" to disable)")
	fs.StringVar(&cfg.IngressHTTPS, "ingress-https", envOr("NUBE_INGRESS_HTTPS", ":443"), "ingress HTTPS address (\"off\" to disable)")
	fs.Parse(args)
	for _, v := range strings.Split(*sans, ",") {
		if v = strings.TrimSpace(v); v != "" {
			cfg.TLSSANs = append(cfg.TLSSANs, v)
		}
	}
	if cfg.IngressHTTP == "off" {
		cfg.IngressHTTP = ""
	}
	if cfg.IngressHTTPS == "off" {
		cfg.IngressHTTPS = ""
	}

	s, err := server.New(cfg)
	if err != nil {
		log.Printf("server: %v", err)
		return 1
	}
	log.Printf("admin token: %s/token", cfg.DataDir)
	ctx, cancel := signalContext()
	defer cancel()
	if err := s.Run(ctx); err != nil {
		log.Printf("server: %v", err)
		return 1
	}
	return 0
}

func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	var cfg agent.Config
	servers := fs.String("server", os.Getenv("NUBE_SERVER"), "control-plane URL(s), comma separated, e.g. https://10.0.0.1:7070,https://10.0.0.2:7070")
	labels := fs.String("labels", os.Getenv("NUBE_LABELS"), "node labels, e.g. zone=home,disk=ssd")
	cpu := fs.Int64("cpu", 0, "override CPU capacity in millicores")
	mem := fs.Int64("memory", 0, "override memory capacity in MiB")
	fs.StringVar(&cfg.Token, "token", os.Getenv("NUBE_TOKEN"), "node token (nube token create NAME --role node)")
	fs.StringVar(&cfg.CAHash, "ca-hash", os.Getenv("NUBE_CA_HASH"), "cluster CA hash for https servers (sha256:..., see /var/lib/nube/ca.hash)")
	fs.BoolVar(&cfg.Insecure, "insecure-skip-tls-verify", os.Getenv("NUBE_INSECURE") == "true", "do not verify the server certificate (testing only)")
	fs.StringVar(&cfg.Name, "name", os.Getenv("NUBE_NODE_NAME"), "node name (default: hostname)")
	fs.StringVar(&cfg.AdvertiseIP, "advertise-ip", os.Getenv("NUBE_ADVERTISE_IP"), "IP of this node reachable by other nodes (default: auto)")
	fs.StringVar(&cfg.Listen, "listen", envOr("NUBE_AGENT_LISTEN", ":7071"), "agent API listen address")
	fs.StringVar(&cfg.DataDir, "data", envOr("NUBE_AGENT_DATA", "/var/lib/nube-agent"), "agent data directory")
	fs.StringVar(&cfg.DockerHost, "docker", os.Getenv("DOCKER_HOST"), "Docker endpoint (default unix:///var/run/docker.sock)")
	fs.StringVar(&cfg.NetworkMode, "network", envOr("NUBE_NETWORK", "wireguard"), "pod overlay: wireguard | host-gw | none")
	fs.IntVar(&cfg.WGPort, "wg-port", 51820, "WireGuard UDP port")
	fs.Parse(args)

	for _, s := range strings.Split(*servers, ",") {
		if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" {
			cfg.Servers = append(cfg.Servers, s)
		}
	}
	cfg.Labels = map[string]string{}
	for _, kv := range strings.Split(*labels, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			cfg.Labels[k] = v
		}
	}
	cfg.CPU, cfg.Memory = *cpu, *mem
	switch cfg.NetworkMode {
	case "wireguard", "host-gw", "none":
	default:
		fmt.Fprintln(os.Stderr, "--network must be wireguard, host-gw or none")
		return 2
	}
	a, err := agent.New(cfg)
	if err != nil {
		log.Printf("agent: %v", err)
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := a.Run(ctx); err != nil && err != context.Canceled {
		log.Printf("agent: %v", err)
		return 1
	}
	return 0
}
