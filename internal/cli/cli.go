package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
	"github.com/thisisnabi/Nube/internal/version"
	"gopkg.in/yaml.v3"
)

// args holds parsed flags and positional arguments.
type args struct {
	flags map[string]string
	multi map[string][]string
	pos   []string
}

var valueFlags = map[string]string{
	"f": "filename", "filename": "filename", "n": "namespace", "namespace": "namespace", "o": "output", "output": "output",
	"server": "server", "token": "token", "replicas": "replicas", "tail": "tail", "to-revision": "to-revision",
	"c": "container", "container": "container", "role": "role", "ca-hash": "ca-hash",
	"to": "to", "name": "name",
}
var boolFlags = map[string]string{
	"A": "all-namespaces", "all-namespaces": "all-namespaces", "w": "watch", "watch": "watch", "force": "force",
	"follow": "follow", "reveal": "reveal", "h": "help", "help": "help",
	"insecure-skip-tls-verify": "insecure-skip-tls-verify", "yes": "yes", "y": "yes",
}

func parseArgs(in []string) (args, error) {
	a := args{flags: map[string]string{}, multi: map[string][]string{}}
	for i := 0; i < len(in); i++ {
		s := in[i]
		if s == "--" {
			a.pos = append(a.pos, in[i+1:]...)
			break
		}
		if !strings.HasPrefix(s, "-") || s == "-" {
			a.pos = append(a.pos, s)
			continue
		}
		name := strings.TrimLeft(s, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		// "logs -f" means follow, everywhere else -f is a filename.
		if name == "f" && len(a.pos) > 0 && (a.pos[0] == "logs" || a.pos[0] == "log") {
			a.flags["follow"] = "true"
			continue
		}
		if canon, ok := boolFlags[name]; ok {
			a.flags[canon] = "true"
			continue
		}
		canon, ok := valueFlags[name]
		if !ok {
			return a, fmt.Errorf("unknown flag %s", s)
		}
		if !hasVal {
			if i+1 >= len(in) {
				return a, fmt.Errorf("flag %s needs a value", s)
			}
			i++
			val = in[i]
		}
		a.flags[canon] = val
		a.multi[canon] = append(a.multi[canon], val)
	}
	return a, nil
}

func (a args) ns() string {
	if a.flags["all-namespaces"] != "" {
		return ""
	}
	if n := a.flags["namespace"]; n != "" {
		return n
	}
	return api.DefaultNamespace
}

const usage = `nube — a small Kubernetes-like container orchestrator

Control plane & nodes:
  nube server [flags]                 run a control-plane server   (nube server -h)
  nube agent  [flags]                 run a worker node agent      (nube agent -h)

Client:
  nube login --server URL --token T [--ca-hash sha256:...]   save connection settings
  nube apply -f FILE|DIR|-            create or update resources from YAML/JSON
  nube delete -f FILE                 delete resources in a manifest
  nube delete KIND NAME [--force]
  nube get KIND [NAME] [-n NS|-A] [-o wide|json|yaml] [-w]
        kinds: nodes deploy sts ds jobs cronjobs pods svc ing netpol cm secrets volumes
               snapshots crd tokens, and the plural of any custom resource
  nube describe KIND NAME
  nube logs POD [-c CONTAINER] [-f] [--tail N]
  nube scale deploy|sts NAME --replicas N
  nube set image deploy|sts NAME IMAGE [-c CONTAINER]
  nube rollout restart|undo|history|status|pause|resume deploy|sts NAME [--to-revision N]
  nube trigger cronjob NAME           run a cronjob now
  nube suspend|resume cronjob NAME
  nube top nodes|pods
  nube cordon|uncordon|drain NODE
  nube label node NAME key=value key-
  nube events [-n NS] [-w]
  nube snapshot VOLUME [--name NAME]   take a snapshot (stored on the control plane)
  nube snapshot download NAME FILE     save a snapshot archive (.tar.gz)
  nube volume move VOLUME --to NODE    move a local volume to another node
  nube token create NAME --role admin|edit|view|node [-n NS ...]
  nube token list | nube token delete NAME
  nube whoami
  nube cluster status                 control-plane members (HA)
  nube cluster remove SERVER_ID
  nube dashboard                      print the dashboard URL
  nube version

Global flags: --server URL --token TOKEN --ca-hash HASH -n NAMESPACE
Config: ~/.nube/config.json, or NUBE_SERVER / NUBE_TOKEN / NUBE_CA_HASH.
`

// Run executes a client command. It returns the process exit code.
func Run(argv []string) int {
	a, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if len(a.pos) == 0 || a.flags["help"] != "" {
		fmt.Print(usage)
		return 0
	}
	c := newClient(a.flags)
	cmd, rest := a.pos[0], a.pos[1:]
	switch cmd {
	case "version":
		fmt.Println("client:", version.Version)
		var v map[string]string
		if err = c.getJSON("/api/v1/version", &v); err == nil {
			fmt.Println("server:", v["version"])
		}
	case "login":
		err = cmdLogin(c, a)
	case "whoami":
		var id api.Identity
		if err = c.getJSON("/api/v1/whoami", &id); err == nil {
			ns := "all namespaces"
			if len(id.Namespaces) > 0 {
				ns = strings.Join(id.Namespaces, ", ")
			}
			fmt.Printf("%s (role %s, %s) at %s\n", id.Name, id.Role, ns, c.cfg.Server)
		}
	case "apply":
		err = cmdApply(c, a, "/api/v1/apply")
	case "delete":
		if a.flags["filename"] != "" {
			path := "/api/v1/delete"
			if a.flags["force"] != "" {
				path += "?force=1"
			}
			err = cmdApply(c, a, path)
		} else {
			err = cmdDelete(c, a, rest)
		}
	case "get":
		err = cmdGet(c, a, rest)
	case "describe":
		err = cmdDescribe(c, a, rest)
	case "logs", "log":
		err = cmdLogs(c, a, rest)
	case "scale":
		err = cmdScale(c, a, rest)
	case "set":
		err = cmdSetImage(c, a, rest)
	case "rollout":
		err = cmdRollout(c, a, rest)
	case "trigger", "suspend", "resume":
		err = cmdCronAction(c, a, cmd, rest)
	case "top":
		err = cmdTop(c, a, rest)
	case "cordon", "uncordon", "drain":
		err = cmdNodeAction(c, cmd, rest)
	case "label":
		err = cmdLabel(c, rest)
	case "events":
		err = cmdEvents(c, a)
	case "token", "tokens":
		err = cmdToken(c, a, rest)
	case "cluster":
		err = cmdCluster(c, rest)
	case "snapshot", "snapshots":
		err = cmdSnapshot(c, a, rest)
	case "volume", "volumes":
		err = cmdVolume(c, a, rest)
	case "dashboard":
		fmt.Printf("Dashboard: %s/\n", c.cfg.Server)
		if c.cfg.CAHash != "" {
			fmt.Printf("CA:        %s/ca.crt  (import it in your browser to avoid the certificate warning)\n", c.cfg.Server)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdLogin(c *client, a args) error {
	cfg := clientConfig{
		Server: strings.TrimRight(a.flags["server"], "/"), Token: a.flags["token"],
		CAHash: a.flags["ca-hash"], Insecure: a.flags["insecure-skip-tls-verify"] != "",
	}
	if cfg.Server == "" || cfg.Token == "" {
		return fmt.Errorf("usage: nube login --server https://SERVER:7070 --token TOKEN [--ca-hash sha256:...]")
	}
	if strings.HasPrefix(cfg.Server, "https://") && cfg.CAHash == "" && !cfg.Insecure {
		// Trust on first use: show the CA fingerprint and ask.
		u, err := url.Parse(cfg.Server)
		if err != nil {
			return err
		}
		host := u.Host
		if u.Port() == "" {
			host += ":443"
		}
		h, err := pki.PeerCAHash(host)
		if err != nil {
			return fmt.Errorf("cannot connect to %s: %v", cfg.Server, err)
		}
		fmt.Printf("The server's certificate authority is\n  %s\n", h)
		fmt.Println("Compare it with /var/lib/nube/ca.hash on the server.")
		if a.flags["yes"] == "" {
			fmt.Print("Trust it? [y/N] ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
				return fmt.Errorf("not trusted; pass --ca-hash to pin it explicitly")
			}
		}
		cfg.CAHash = h
	}
	c.cfg = cfg
	c.rebuild(30 * time.Second)
	var id api.Identity
	if err := c.getJSON("/api/v1/whoami", &id); err != nil {
		return err
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Logged in to %s as %s (role %s). Saved to %s\n", cfg.Server, id.Name, id.Role, configPath())
	return nil
}

// readManifests reads every -f argument: files, directories (*.yaml|*.yml|*.json) or "-" for stdin.
func readManifests(paths []string) ([]byte, error) {
	var out []byte
	add := func(b []byte) {
		if len(out) > 0 {
			out = append(out, []byte("\n---\n")...)
		}
		out = append(out, b...)
	}
	for _, p := range paths {
		if p == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, err
			}
			add(b)
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			add(b)
			continue
		}
		entries, _ := os.ReadDir(p)
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if e.IsDir() || (ext != ".yaml" && ext != ".yml" && ext != ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(p, e.Name()))
			if err != nil {
				return nil, err
			}
			add(b)
		}
	}
	return out, nil
}

func cmdApply(c *client, a args, path string) error {
	files := a.multi["filename"]
	if len(files) == 0 {
		return fmt.Errorf("-f FILE is required")
	}
	body, err := readManifests(files)
	if err != nil {
		return err
	}
	if n := a.flags["namespace"]; n != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "namespace=" + url.QueryEscape(n)
	}
	var out struct {
		Results []string `json:"results"`
	}
	if err := c.send("POST", path, body, "application/yaml", &out); err != nil {
		return err
	}
	for _, r := range out.Results {
		fmt.Println(r)
	}
	return nil
}

func kindArg(rest []string) (string, []string, error) {
	if len(rest) == 0 {
		return "", nil, fmt.Errorf("resource type required (nodes, deploy, sts, jobs, cronjobs, pods, svc, ing, cm, secrets, volumes)")
	}
	kindStr, rest := rest[0], rest[1:]
	if k, n, ok := strings.Cut(kindStr, "/"); ok {
		kindStr = k
		rest = append([]string{n}, rest...)
	}
	kind := api.NormalizeKind(kindStr)
	if kind == "" {
		// A custom resource: the server resolves its plural/kind/short name.
		if !api.ValidName(strings.ToLower(kindStr)) {
			return "", nil, fmt.Errorf("unknown resource type %q", kindStr)
		}
		return "custom:" + strings.ToLower(kindStr), rest, nil
	}
	return kind, rest, nil
}

func plural(kind string) string {
	if p, ok := strings.CutPrefix(kind, "custom:"); ok {
		return p
	}
	return api.Plural(kind)
}

func objectPath(kind, ns, name string) string {
	if kind == api.KindNode {
		return "/api/v1/nodes/" + url.PathEscape(name)
	}
	if kind == api.KindCRD {
		ns = "-"
	}
	return "/api/v1/namespaces/" + url.PathEscape(ns) + "/" + plural(kind) + "/" + url.PathEscape(name)
}

func cmdDelete(c *client, a args, rest []string) error {
	kind, rest, err := kindArg(rest)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("name required")
	}
	if kind == api.KindToken {
		return cmdToken(c, a, append([]string{"delete"}, rest...))
	}
	for _, name := range rest {
		path := objectPath(kind, a.ns(), name)
		if a.flags["force"] != "" {
			path += "?force=1"
		}
		var out map[string]string
		if err := c.send("DELETE", path, nil, "", &out); err != nil {
			return err
		}
		fmt.Println(out["status"])
	}
	return nil
}

// ---------------------------------------------------------------- get

func cmdGet(c *client, a args, rest []string) error {
	kind, rest, err := kindArg(rest)
	if err != nil {
		return err
	}
	if kind == api.KindToken {
		return cmdToken(c, a, []string{"list"})
	}
	for {
		if err := getOnce(c, a, kind, rest); err != nil {
			return err
		}
		if a.flags["watch"] == "" {
			return nil
		}
		time.Sleep(2 * time.Second)
		fmt.Print("\033[H\033[2J")
	}
}

func getOnce(c *client, a args, kind string, rest []string) error {
	ns := a.ns()
	q := ""
	if a.flags["reveal"] != "" {
		q = "?reveal=1"
	}
	if len(rest) > 0 {
		var obj json.RawMessage
		if err := c.getJSON(objectPath(kind, ns, rest[0])+q, &obj); err != nil {
			return err
		}
		if a.flags["output"] == "" || a.flags["output"] == "wide" {
			if strings.HasPrefix(kind, "custom:") {
				return printCustom(c, kind, []json.RawMessage{obj}, ns == "")
			}
			return printTable(kind, []json.RawMessage{obj}, a.flags["output"] == "wide", ns == "")
		}
		return printRaw(obj, a.flags["output"])
	}
	path := "/api/v1/" + plural(kind)
	if kind != api.KindNode && kind != api.KindCRD && ns != "" {
		path = "/api/v1/namespaces/" + url.PathEscape(ns) + "/" + plural(kind)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := c.getJSON(path+q, &list); err != nil {
		return err
	}
	switch a.flags["output"] {
	case "json", "yaml":
		b, _ := json.Marshal(list.Items)
		return printRaw(b, a.flags["output"])
	}
	if len(list.Items) == 0 {
		where := "namespace " + ns
		if ns == "" || kind == api.KindNode {
			where = "cluster"
		}
		fmt.Printf("No %s found in %s.\n", plural(kind), where)
		return nil
	}
	if strings.HasPrefix(kind, "custom:") {
		return printCustom(c, kind, list.Items, ns == "")
	}
	return printTable(kind, list.Items, a.flags["output"] == "wide", ns == "")
}

func printRaw(raw []byte, format string) error {
	var v any
	json.Unmarshal(raw, &v)
	if format == "yaml" {
		enc := yaml.NewEncoder(os.Stdout)
		enc.SetIndent(2)
		return enc.Encode(v)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
	return nil
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func newTab() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0) }

func pct(used, total int64) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", used*100/total)
}

func podReady(p api.Pod) string {
	n := len(p.Spec.Template.AllContainers())
	ready := 0
	for _, c := range p.Status.Containers {
		if !c.Init && c.Ready {
			ready++
		}
	}
	if len(p.Status.Containers) == 0 && p.Status.Ready {
		ready = n
	}
	return fmt.Sprintf("%d/%d", ready, n)
}

func podStatus(p api.Pod) string {
	switch {
	case p.Status.Phase == api.PodCrashLoop:
		return "CrashLoopBackOff"
	case p.Status.Phase == api.PodSucceeded:
		return "Completed"
	case p.Status.Phase == api.PodInit:
		done := 0
		for _, c := range p.Status.Containers {
			if c.Init && c.State == "Terminated" && c.ExitCode == 0 {
				done++
			}
		}
		return fmt.Sprintf("Init:%d/%d", done, len(p.Spec.Template.InitContainers))
	}
	return string(p.Status.Phase)
}

func printTable(kind string, items []json.RawMessage, wide, showNS bool) error {
	w := newTab()
	defer w.Flush()
	nsCol := func(ns string) string {
		if showNS {
			return ns + "\t"
		}
		return ""
	}
	nsHead := ""
	if showNS {
		nsHead = "NAMESPACE\t"
	}
	extra := func(cols string) string {
		if wide {
			return cols
		}
		return ""
	}
	switch kind {
	case api.KindNode:
		fmt.Fprintln(w, "NAME\tSTATUS\tADDRESS\tPODS\tCPU(req)\tMEM(req)\tCPU(use)\tMEM(use)\tNETWORK\tAGE"+extra("\tVERSION\tOS"))
		for _, raw := range items {
			var n api.Node
			json.Unmarshal(raw, &n)
			status := string(n.Status)
			if n.Cordoned {
				status += ",SchedulingDisabled"
			}
			netw := n.NetworkMode
			if n.NetworkMode != "" && n.NetworkMode != "none" && !n.NetworkReady {
				netw += "(down)"
			}
			line := fmt.Sprintf("%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s", n.Name, status, n.HostIP, n.PodCount,
				pct(n.Allocated.CPU, n.Capacity.CPU), pct(n.Allocated.Memory, n.Capacity.Memory),
				pct(n.Usage.CPU, n.Capacity.CPU), pct(n.Usage.Memory, n.Capacity.Memory), netw, age(n.JoinedAt))
			if wide {
				line += "\t" + n.Info.AgentVersion + "\t" + n.Info.OS
			}
			fmt.Fprintln(w, line)
		}
	case api.KindDeployment:
		fmt.Fprintln(w, nsHead+"NAME\tREADY\tUP-TO-DATE\tSTATUS\tREVISION\tIMAGE\tAGE"+extra("\tMESSAGE"))
		for _, raw := range items {
			var d api.Deployment
			json.Unmarshal(raw, &d)
			line := fmt.Sprintf("%s%s\t%d/%d\t%d\t%s\t%d\t%s\t%s", nsCol(d.Metadata.Namespace), d.Metadata.Name,
				d.Status.ReadyReplicas, d.Spec.Replicas, d.Status.UpdatedReplicas, d.Status.Phase, d.Status.Revision,
				d.Spec.Template.MainImage(), age(d.Metadata.CreatedAt))
			if wide {
				line += "\t" + d.Status.Message
			}
			fmt.Fprintln(w, line)
		}
	case api.KindStatefulSet:
		fmt.Fprintln(w, nsHead+"NAME\tREADY\tUP-TO-DATE\tSTATUS\tSERVICE\tIMAGE\tAGE"+extra("\tMESSAGE"))
		for _, raw := range items {
			var s api.StatefulSet
			json.Unmarshal(raw, &s)
			line := fmt.Sprintf("%s%s\t%d/%d\t%d\t%s\t%s\t%s\t%s", nsCol(s.Metadata.Namespace), s.Metadata.Name,
				s.Status.ReadyReplicas, s.Spec.Replicas, s.Status.UpdatedReplicas, s.Status.Phase, firstNonEmpty(s.Spec.ServiceName, "-"),
				s.Spec.Template.MainImage(), age(s.Metadata.CreatedAt))
			if wide {
				line += "\t" + s.Status.Message
			}
			fmt.Fprintln(w, line)
		}
	case api.KindJob:
		fmt.Fprintln(w, nsHead+"NAME\tSTATUS\tCOMPLETIONS\tFAILED\tDURATION\tAGE"+extra("\tMESSAGE"))
		for _, raw := range items {
			var j api.Job
			json.Unmarshal(raw, &j)
			dur := "-"
			if !j.Status.StartTime.IsZero() {
				end := j.Status.CompletionTime
				if end.IsZero() {
					end = time.Now()
				}
				dur = end.Sub(j.Status.StartTime).Round(time.Second).String()
			}
			line := fmt.Sprintf("%s%s\t%s\t%d/%d\t%d\t%s\t%s", nsCol(j.Metadata.Namespace), j.Metadata.Name, j.Status.Phase,
				j.Status.Succeeded, j.Spec.Completions, j.Status.Failed, dur, age(j.Metadata.CreatedAt))
			if wide {
				line += "\t" + j.Status.Message
			}
			fmt.Fprintln(w, line)
		}
	case api.KindCronJob:
		fmt.Fprintln(w, nsHead+"NAME\tSCHEDULE\tSUSPEND\tACTIVE\tLAST\tNEXT\tAGE")
		for _, raw := range items {
			var cj api.CronJob
			json.Unmarshal(raw, &cj)
			last, next := "-", "-"
			if !cj.Status.LastScheduleTime.IsZero() {
				last = age(cj.Status.LastScheduleTime) + " ago"
			}
			if !cj.Status.NextScheduleTime.IsZero() && !cj.Spec.Suspend {
				next = "in " + age(cj.Status.NextScheduleTime)
			}
			fmt.Fprintf(w, "%s%s\t%s\t%v\t%d\t%s\t%s\t%s\n", nsCol(cj.Metadata.Namespace), cj.Metadata.Name, cj.Spec.Schedule,
				cj.Spec.Suspend, len(cj.Status.Active), last, next, age(cj.Metadata.CreatedAt))
		}
	case api.KindPod:
		fmt.Fprintln(w, nsHead+"NAME\tREADY\tSTATUS\tRESTARTS\tNODE\tIP\tAGE"+extra("\tCPU\tMEM\tMESSAGE"))
		for _, raw := range items {
			var p api.Pod
			json.Unmarshal(raw, &p)
			node := firstNonEmpty(p.Spec.NodeName, "<none>")
			ip := firstNonEmpty(p.Status.PodIP, "-")
			line := fmt.Sprintf("%s%s\t%s\t%s\t%d\t%s\t%s\t%s", nsCol(p.Metadata.Namespace), p.Metadata.Name, podReady(p), podStatus(p),
				p.Status.Restarts, node, ip, age(p.Metadata.CreatedAt))
			if wide {
				line += fmt.Sprintf("\t%dm\t%dMi\t%s", p.Status.CPU, p.Status.Memory, p.Status.Message)
			}
			fmt.Fprintln(w, line)
		}
	case api.KindService:
		fmt.Fprintln(w, nsHead+"NAME\tTYPE\tPORTS\tSELECTOR\tENDPOINTS\tAGE")
		for _, raw := range items {
			var s api.Service
			json.Unmarshal(raw, &s)
			var ports []string
			for _, p := range s.Spec.Ports {
				pp := fmt.Sprintf("%d->%d", p.Port, p.TargetPort)
				if p.IsUDP() {
					pp += "/udp"
				}
				ports = append(ports, pp)
			}
			typ := "LoadBalancer"
			switch {
			case s.Spec.Headless:
				typ = "Headless"
			case s.Spec.Internal:
				typ = "Internal"
			}
			eps := strings.Join(s.Status.Endpoints, ",")
			if eps == "" {
				eps = "<none>"
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%s\n", nsCol(s.Metadata.Namespace), s.Metadata.Name, typ, strings.Join(ports, ","),
				labelString(s.Spec.Selector), eps, age(s.Metadata.CreatedAt))
		}
	case api.KindIngress:
		fmt.Fprintln(w, nsHead+"NAME\tHOSTS\tROUTES\tTLS\tREQUESTS\tAGE"+extra("\tMESSAGE"))
		for _, raw := range items {
			var ing api.Ingress
			json.Unmarshal(raw, &ing)
			var hosts, routes []string
			for _, r := range ing.Spec.Rules {
				hosts = append(hosts, firstNonEmpty(r.Host, "*"))
				for _, p := range r.Paths {
					routes = append(routes, fmt.Sprintf("%s%s->%s:%d", firstNonEmpty(r.Host, ""), p.Path, p.Backend.Service, p.Backend.Port))
				}
			}
			tlsHosts := "-"
			if len(ing.Spec.TLS) > 0 {
				var th []string
				for _, t := range ing.Spec.TLS {
					th = append(th, t.Hosts...)
				}
				tlsHosts = strings.Join(th, ",")
			}
			line := fmt.Sprintf("%s%s\t%s\t%s\t%s\t%d\t%s", nsCol(ing.Metadata.Namespace), ing.Metadata.Name, strings.Join(dedupe(hosts), ","),
				strings.Join(routes, " "), tlsHosts, ing.Status.Requests, age(ing.Metadata.CreatedAt))
			if wide {
				line += "\t" + ing.Status.Message
			}
			fmt.Fprintln(w, line)
		}
	case api.KindConfigMap, api.KindSecret:
		fmt.Fprintln(w, nsHead+"NAME\tKEYS\tAGE")
		for _, raw := range items {
			var cm api.ConfigMap
			json.Unmarshal(raw, &cm)
			keys := make([]string, 0, len(cm.Data))
			for k := range cm.Data {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Fprintf(w, "%s%s\t%s\t%s\n", nsCol(cm.Metadata.Namespace), cm.Metadata.Name, strings.Join(keys, ","), age(cm.Metadata.CreatedAt))
		}
	case api.KindDaemonSet:
		fmt.Fprintln(w, nsHead+"NAME\tDESIRED\tCURRENT\tREADY\tUP-TO-DATE\tSTATUS\tIMAGE\tAGE")
		for _, raw := range items {
			var d api.DaemonSet
			json.Unmarshal(raw, &d)
			fmt.Fprintf(w, "%s%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", nsCol(d.Metadata.Namespace), d.Metadata.Name, d.Status.DesiredNumber,
				d.Status.Current, d.Status.Ready, d.Status.Updated, d.Status.Phase, d.Spec.Template.MainImage(), age(d.Metadata.CreatedAt))
		}
	case api.KindNetworkPolicy:
		fmt.Fprintln(w, nsHead+"NAME\tPOD-SELECTOR\tTYPES\tPODS\tAGE")
		for _, raw := range items {
			var np api.NetworkPolicy
			json.Unmarshal(raw, &np)
			sel := labelString(np.Spec.PodSelector.MatchLabels)
			if sel == "" {
				sel = "<all pods>"
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%d\t%s\n", nsCol(np.Metadata.Namespace), np.Metadata.Name, sel,
				strings.Join(np.Spec.PolicyTypes, ","), np.Status.SelectedPods, age(np.Metadata.CreatedAt))
		}
	case api.KindVolumeSnapshot:
		fmt.Fprintln(w, nsHead+"NAME\tVOLUME\tSTATUS\tSIZE\tTAKEN\tCOPIES\tMESSAGE")
		for _, raw := range items {
			var s api.VolumeSnapshot
			json.Unmarshal(raw, &s)
			taken := "-"
			if !s.Status.TakenAt.IsZero() {
				taken = age(s.Status.TakenAt) + " ago"
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%d\t%s\n", nsCol(s.Metadata.Namespace), s.Metadata.Name, s.Spec.Volume,
				s.Status.Phase, humanSize(s.Status.SizeBytes), taken, len(s.Status.StoredOn), s.Status.Message)
		}
	case api.KindCRD:
		fmt.Fprintln(w, "NAME\tKIND\tPLURAL\tSHORT\tOBJECTS\tAGE")
		for _, raw := range items {
			var c api.CustomResourceDefinition
			json.Unmarshal(raw, &c)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", c.Metadata.Name, c.Spec.Kind, c.Spec.Plural,
				firstNonEmpty(strings.Join(c.Spec.ShortNames, ","), "-"), c.Status.Objects, age(c.Metadata.CreatedAt))
		}
	case api.KindVolume:
		fmt.Fprintln(w, nsHead+"NAME\tTYPE\tSIZE\tUSED\tSTATUS\tNODE\tUSED-BY\tAGE")
		for _, raw := range items {
			var v api.Volume
			json.Unmarshal(raw, &v)
			used := "-"
			if v.Status.CapacityMiB > 0 {
				used = fmt.Sprintf("%dMi (%s)", v.Status.UsedMiB, pct(v.Status.UsedMiB, v.Status.CapacityMiB))
			}
			size := firstNonEmpty(v.Spec.Size, "-")
			if v.Spec.Size != "" && !v.Status.Enforced && v.Status.NodeName != "" {
				size += "*"
			}
			fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", nsCol(v.Metadata.Namespace), v.Metadata.Name, v.Spec.Type,
				size, used, v.Status.Phase, firstNonEmpty(v.Status.NodeName, "-"),
				firstNonEmpty(strings.Join(v.Status.UsedBy, ","), "-"), age(v.Metadata.CreatedAt))
		}
	}
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func labelString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------- describe

func cmdDescribe(c *client, a args, rest []string) error {
	kind, rest, err := kindArg(rest)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("name required")
	}
	ns, name := a.ns(), rest[0]
	var raw json.RawMessage
	if err := c.getJSON(objectPath(kind, ns, name), &raw); err != nil {
		return err
	}
	if err := printRaw(raw, "yaml"); err != nil {
		return err
	}
	var events []api.Event
	if err := c.getJSON("/api/v1/events", &events); err != nil {
		return nil
	}
	fmt.Println("\nEvents:")
	w := newTab()
	fmt.Fprintln(w, "  AGE\tTYPE\tREASON\tMESSAGE")
	shown := 0
	for _, e := range events {
		related := e.Kind == kind && e.Name == name
		if (kind == api.KindDeployment || kind == api.KindStatefulSet || kind == api.KindJob) && e.Kind == api.KindPod && strings.HasPrefix(e.Name, name+"-") {
			related = true
		}
		if related && (kind == api.KindNode || e.Namespace == ns) {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", age(e.Time), e.Type, e.Reason, e.Message)
			shown++
		}
	}
	if shown == 0 {
		fmt.Fprintln(w, "  <none>")
	}
	return w.Flush()
}

// ---------------------------------------------------------------- logs, scale, rollout

func cmdLogs(c *client, a args, rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("usage: nube logs POD [-c CONTAINER] [-f] [--tail N]")
	}
	name := strings.TrimPrefix(rest[0], "pod/")
	q := url.Values{}
	q.Set("tail", firstNonEmpty(a.flags["tail"], "200"))
	if a.flags["follow"] != "" {
		q.Set("follow", "1")
	}
	if ct := a.flags["container"]; ct != "" {
		q.Set("container", ct)
	}
	c.rebuild(0) // no timeout: follow streams
	resp, err := c.request("GET", "/api/v1/namespaces/"+url.PathEscape(a.ns())+"/pods/"+url.PathEscape(name)+"/logs?"+q.Encode(), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

// workloadArg parses "deploy NAME", "sts/NAME" or "NAME" (defaults to deployment).
func workloadArg(rest []string) (kind, name string, err error) {
	if len(rest) == 0 {
		return "", "", fmt.Errorf("workload name required")
	}
	if k, n, ok := strings.Cut(rest[0], "/"); ok {
		kind, name = api.NormalizeKind(k), n
	} else if k := api.NormalizeKind(rest[0]); k != "" {
		if len(rest) < 2 {
			return "", "", fmt.Errorf("name required")
		}
		kind, name = k, rest[1]
	} else {
		kind, name = api.KindDeployment, rest[0]
	}
	if kind != api.KindDeployment && kind != api.KindStatefulSet {
		return "", "", fmt.Errorf("only deployments and statefulsets support this")
	}
	return kind, name, nil
}

func workloadAction(c *client, a args, kind, name, action string, body any) error {
	var out map[string]string
	path := objectPath(kind, a.ns(), name) + "/" + action
	if err := c.postJSON(path, body, &out); err != nil {
		return err
	}
	fmt.Printf("%s/%s: %s\n", strings.ToLower(kind), name, out["status"])
	return nil
}

func cmdScale(c *client, a args, rest []string) error {
	kind, name, err := workloadArg(rest)
	if err != nil {
		return err
	}
	r := a.flags["replicas"]
	if r == "" && len(rest) > 1 {
		r = rest[len(rest)-1]
	}
	n, err := strconv.Atoi(r)
	if err != nil {
		return fmt.Errorf("usage: nube scale deploy|sts NAME --replicas N")
	}
	return workloadAction(c, a, kind, name, "scale", map[string]int{"replicas": n})
}

func cmdSetImage(c *client, a args, rest []string) error {
	if len(rest) < 3 || rest[0] != "image" {
		return fmt.Errorf("usage: nube set image deploy|sts NAME IMAGE [-c CONTAINER]")
	}
	kind, name, err := workloadArg(rest[1 : len(rest)-1])
	if err != nil {
		return err
	}
	return workloadAction(c, a, kind, name, "set-image", map[string]string{"image": rest[len(rest)-1], "container": a.flags["container"]})
}

func cmdRollout(c *client, a args, rest []string) error {
	if len(rest) < 2 {
		return fmt.Errorf("usage: nube rollout restart|undo|history|status|pause|resume deploy|sts NAME")
	}
	sub := rest[0]
	kind, name, err := workloadArg(rest[1:])
	if err != nil {
		return err
	}
	switch sub {
	case "restart", "pause", "resume":
		return workloadAction(c, a, kind, name, sub, map[string]any{})
	case "undo":
		if kind != api.KindDeployment {
			return fmt.Errorf("undo is supported for deployments")
		}
		rev, _ := strconv.Atoi(a.flags["to-revision"])
		return workloadAction(c, a, kind, name, "rollback", map[string]int{"revision": rev})
	case "status":
		if kind == api.KindStatefulSet {
			var s api.StatefulSet
			if err := c.getJSON(objectPath(kind, a.ns(), name), &s); err != nil {
				return err
			}
			fmt.Printf("statefulset/%s: %s — %d/%d ready, %d updated\n", name, s.Status.Phase, s.Status.ReadyReplicas, s.Spec.Replicas, s.Status.UpdatedReplicas)
			return nil
		}
		var d api.Deployment
		if err := c.getJSON(objectPath(kind, a.ns(), name), &d); err != nil {
			return err
		}
		fmt.Printf("deployment/%s: %s — %d/%d ready, %d up-to-date, revision %d\n", name, d.Status.Phase,
			d.Status.ReadyReplicas, d.Spec.Replicas, d.Status.UpdatedReplicas, d.Status.Revision)
		if d.Status.Message != "" {
			fmt.Println("  " + d.Status.Message)
		}
		return nil
	case "history":
		var d api.Deployment
		if err := c.getJSON(objectPath(api.KindDeployment, a.ns(), name), &d); err != nil {
			return err
		}
		w := newTab()
		fmt.Fprintln(w, "REVISION\tIMAGE\tCREATED\tCURRENT")
		for _, h := range d.Status.History {
			cur := ""
			if h.Hash == d.Status.CurrentHash {
				cur = "*"
			}
			fmt.Fprintf(w, "%d\t%s\t%s ago\t%s\n", h.Revision, h.Template.MainImage(), age(h.CreatedAt), cur)
		}
		return w.Flush()
	}
	return fmt.Errorf("unknown rollout command %q", sub)
}

func cmdCronAction(c *client, a args, action string, rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("usage: nube %s cronjob NAME", action)
	}
	name := rest[len(rest)-1]
	if k, n, ok := strings.Cut(rest[0], "/"); ok && api.NormalizeKind(k) == api.KindCronJob {
		name = n
	} else if api.NormalizeKind(rest[0]) != api.KindCronJob || len(rest) < 2 {
		return fmt.Errorf("usage: nube %s cronjob NAME", action)
	}
	var out map[string]string
	if err := c.postJSON(objectPath(api.KindCronJob, a.ns(), name)+"/"+action, map[string]any{}, &out); err != nil {
		return err
	}
	fmt.Printf("cronjob/%s: %s\n", name, out["status"])
	return nil
}

// ---------------------------------------------------------------- nodes, top, events, tokens, cluster

func cmdNodeAction(c *client, action string, rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("node name required")
	}
	var out map[string]string
	if err := c.postJSON("/api/v1/nodes/"+url.PathEscape(rest[0])+"/"+action, map[string]any{}, &out); err != nil {
		return err
	}
	fmt.Printf("node/%s: %s\n", rest[0], out["status"])
	return nil
}

func cmdLabel(c *client, rest []string) error {
	if len(rest) < 3 || api.NormalizeKind(rest[0]) != api.KindNode {
		return fmt.Errorf("usage: nube label node NAME key=value [key-]")
	}
	labels := map[string]string{}
	for _, kv := range rest[2:] {
		if k, v, ok := strings.Cut(kv, "="); ok {
			labels[k] = v
		} else if strings.HasSuffix(kv, "-") {
			labels[strings.TrimSuffix(kv, "-")] = ""
		}
	}
	var out map[string]string
	if err := c.postJSON("/api/v1/nodes/"+url.PathEscape(rest[1])+"/labels", map[string]any{"labels": labels}, &out); err != nil {
		return err
	}
	fmt.Printf("node/%s: %s\n", rest[1], out["status"])
	return nil
}

func cmdTop(c *client, a args, rest []string) error {
	var ov api.Overview
	if err := c.getJSON("/api/v1/overview", &ov); err != nil {
		return err
	}
	w := newTab()
	defer w.Flush()
	if len(rest) > 0 && api.NormalizeKind(rest[0]) == api.KindPod {
		fmt.Fprintln(w, "NAMESPACE\tPOD\tNODE\tCPU\tMEMORY")
		for _, p := range ov.Pods {
			if a.flags["all-namespaces"] == "" && p.Metadata.Namespace != a.ns() {
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%dm\t%dMi\n", p.Metadata.Namespace, p.Metadata.Name, p.Spec.NodeName, p.Status.CPU, p.Status.Memory)
		}
		return nil
	}
	fmt.Fprintln(w, "NODE\tCPU\tCPU%\tMEMORY\tMEMORY%")
	for _, n := range ov.Nodes {
		fmt.Fprintf(w, "%s\t%dm\t%s\t%dMi\t%s\n", n.Name, n.Usage.CPU, pct(n.Usage.CPU, n.Capacity.CPU), n.Usage.Memory, pct(n.Usage.Memory, n.Capacity.Memory))
	}
	return nil
}

func cmdEvents(c *client, a args) error {
	var since int64
	ns := a.flags["namespace"]
	first := true
	for {
		var events []api.Event
		if err := c.getJSON("/api/v1/events?since="+strconv.FormatInt(since, 10), &events); err != nil {
			return err
		}
		w := newTab()
		if first {
			fmt.Fprintln(w, "AGE\tTYPE\tOBJECT\tREASON\tMESSAGE")
			first = false
		}
		for _, e := range events {
			since = max(since, e.ID)
			if ns != "" && e.Namespace != ns && e.Namespace != "" {
				continue
			}
			obj := strings.ToLower(e.Kind) + "/" + e.Name
			if e.Namespace != "" && e.Namespace != api.DefaultNamespace {
				obj = e.Namespace + "/" + obj
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", age(e.Time), e.Type, obj, e.Reason, e.Message)
		}
		w.Flush()
		if a.flags["watch"] == "" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

func cmdToken(c *client, a args, rest []string) error {
	sub := "list"
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "list", "ls":
		var out struct {
			Items []api.Token `json:"items"`
		}
		if err := c.getJSON("/api/v1/tokens", &out); err != nil {
			return err
		}
		w := newTab()
		fmt.Fprintln(w, "NAME\tROLE\tNAMESPACES\tID\tAGE")
		for _, t := range out.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Role, firstNonEmpty(strings.Join(t.Namespaces, ","), "*"), t.ID, age(t.CreatedAt))
		}
		return w.Flush()
	case "create":
		if len(rest) < 2 || a.flags["role"] == "" {
			return fmt.Errorf("usage: nube token create NAME --role admin|edit|view|node [-n NAMESPACE ...]")
		}
		var t api.Token
		if err := c.postJSON("/api/v1/tokens", api.Token{Name: rest[1], Role: a.flags["role"], Namespaces: a.multi["namespace"]}, &t); err != nil {
			return err
		}
		fmt.Printf("token/%s created (role %s)\n\n  %s\n\nStore it now: it cannot be shown again.\n", t.Name, t.Role, t.Secret)
		return nil
	case "delete", "rm", "revoke":
		if len(rest) < 2 {
			return fmt.Errorf("usage: nube token delete NAME")
		}
		var out map[string]string
		if err := c.send("DELETE", "/api/v1/tokens/"+url.PathEscape(rest[1]), nil, "", &out); err != nil {
			return err
		}
		fmt.Println(out["status"])
		return nil
	}
	return fmt.Errorf("usage: nube token list|create|delete")
}

func cmdCluster(c *client, rest []string) error {
	if len(rest) == 0 || rest[0] == "status" {
		var ci api.ClusterInfo
		if err := c.getJSON("/api/v1/cluster", &ci); err != nil {
			return err
		}
		fmt.Printf("Mode: %s   Leader: %s   Term: %d   (answered by %s, %s)\n", ci.Mode, ci.Leader, ci.Term, ci.ServerID, ci.State)
		if ci.TLS {
			fmt.Printf("TLS:  on, cluster CA %s\n", firstNonEmpty(ci.CAHash, "(not created yet)"))
		} else {
			fmt.Println("TLS:  off")
		}
		fmt.Println()
		if ci.Mode != "ha" {
			fmt.Println("Single-server control plane. Start servers with --ha to enable high availability.")
			return nil
		}
		w := newTab()
		fmt.Fprintln(w, "SERVER\tROLE\tRAFT\tAPI\tVOTER")
		for _, m := range ci.Members {
			role := "follower"
			if m.Leader {
				role = "leader"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n", m.ID, role, m.RaftAddr, m.APIAddr, m.Voter)
		}
		return w.Flush()
	}
	if rest[0] == "remove" && len(rest) == 2 {
		var out map[string]string
		if err := c.send("DELETE", "/api/v1/cluster/servers/"+url.PathEscape(rest[1]), nil, "", &out); err != nil {
			return err
		}
		fmt.Println("server", rest[1], out["status"])
		return nil
	}
	return fmt.Errorf("usage: nube cluster status | nube cluster remove SERVER_ID")
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMi", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKi", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// printCustom prints custom objects using the printer columns of their CRD.
func printCustom(c *client, kind string, items []json.RawMessage, showNS bool) error {
	var crds struct {
		Items []api.CustomResourceDefinition `json:"items"`
	}
	c.getJSON("/api/v1/customresourcedefinitions", &crds)
	name := strings.TrimPrefix(kind, "custom:")
	var cols []api.PrinterColumn
	for _, d := range crds.Items {
		if d.Spec.Plural == name || strings.EqualFold(d.Spec.Kind, name) || contains(d.Spec.ShortNames, name) {
			cols = d.Spec.PrinterColumns
		}
	}
	w := newTab()
	defer w.Flush()
	head := "NAME"
	if showNS {
		head = "NAMESPACE\tNAME"
	}
	for _, col := range cols {
		head += "\t" + strings.ToUpper(col.Name)
	}
	fmt.Fprintln(w, head+"\tSYNCED\tAGE")
	for _, raw := range items {
		var o api.CustomObject
		json.Unmarshal(raw, &o)
		var doc map[string]any
		json.Unmarshal(raw, &doc)
		line := o.Metadata.Name
		if showNS {
			line = o.Metadata.Namespace + "\t" + line
		}
		for _, col := range cols {
			line += "\t" + lookupPath(doc, col.Path)
		}
		synced := "no"
		if o.Observed >= o.Generation {
			synced = "yes"
		}
		fmt.Fprintln(w, line+"\t"+synced+"\t"+age(o.Metadata.CreatedAt))
	}
	return nil
}

func lookupPath(doc map[string]any, path string) string {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "-"
		}
		cur = m[part]
	}
	if cur == nil {
		return "-"
	}
	switch v := cur.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return v
	}
	b, _ := json.Marshal(cur)
	return string(b)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func cmdSnapshot(c *client, a args, rest []string) error {
	if len(rest) >= 1 && rest[0] == "download" {
		if len(rest) < 3 {
			return fmt.Errorf("usage: nube snapshot download NAME FILE")
		}
		c.rebuild(0)
		resp, err := c.request("GET", "/api/v1/namespaces/"+url.PathEscape(a.ns())+"/volumesnapshots/"+url.PathEscape(rest[1])+"/data", nil, "")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		f, err := os.Create(rest[2])
		if err != nil {
			return err
		}
		n, err := io.Copy(f, resp.Body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		fmt.Printf("saved %s (%s, sha256 %s)\n", rest[2], humanSize(n), resp.Header.Get("X-Nube-SHA256"))
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: nube snapshot VOLUME [--name NAME] | nube snapshot download NAME FILE")
	}
	name := a.flags["name"]
	if name == "" {
		name = rest[0] + "-" + time.Now().UTC().Format("20060102-150405")
	}
	manifest := fmt.Sprintf("kind: VolumeSnapshot\nmetadata: {name: %s, namespace: %s}\nspec: {volume: %s}\n", name, a.ns(), rest[0])
	var out struct {
		Results []string `json:"results"`
	}
	if err := c.send("POST", "/api/v1/apply", []byte(manifest), "application/yaml", &out); err != nil {
		return err
	}
	fmt.Printf("volumesnapshot/%s requested; watch it with: nube get snapshots\n", name)
	return nil
}

func cmdVolume(c *client, a args, rest []string) error {
	if len(rest) != 2 || rest[0] != "move" || a.flags["to"] == "" {
		return fmt.Errorf("usage: nube volume move VOLUME --to NODE")
	}
	var out map[string]string
	if err := c.postJSON("/api/v1/namespaces/"+url.PathEscape(a.ns())+"/volumes/"+url.PathEscape(rest[1])+"/move", map[string]string{"node": a.flags["to"]}, &out); err != nil {
		return err
	}
	fmt.Println(out["status"])
	return nil
}
