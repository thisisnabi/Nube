// Command website-operator is an example operator for Nube: for every
// Website object it runs a Deployment and a Service (and an Ingress when a
// host is set), and reports the result in the Website's status.
//
//	nube apply -f examples/operator/website-crd.yaml
//	NUBE_API_SERVER=https://SERVER:7070 NUBE_TOKEN=... NUBE_CA_HASH=sha256:... go run ./examples/operator
//	nube apply -f examples/operator/website.yaml
//	nube get websites
//
// Everything it creates carries the label nube/owned-by=Website/<name>, so
// deleting the Website deletes them too.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thisisnabi/Nube/pkg/client"
)

type websiteSpec struct {
	Image    string  `json:"image"`
	Replicas float64 `json:"replicas"`
	Port     float64 `json:"port"`
	Host     string  `json:"host"`
}

type websiteStatus struct {
	Phase         string `json:"phase"`
	ReadyReplicas int    `json:"readyReplicas"`
	URL           string `json:"url,omitempty"`
}

func main() {
	c, err := client.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("website-operator watching websites on %s", c.Server)
	c.Run(ctx, "websites", 15*time.Second, func(ctx context.Context, w *client.CustomObject, deleted bool) error {
		if deleted {
			log.Printf("website %s/%s deleted (owned objects are garbage-collected)", w.Metadata.Namespace, w.Metadata.Name)
			return nil
		}
		return reconcile(ctx, c, w)
	})
}

func reconcile(ctx context.Context, c *client.Client, w *client.CustomObject) error {
	var spec websiteSpec
	if err := json.Unmarshal(w.Spec, &spec); err != nil {
		return err
	}
	ns, name := w.Metadata.Namespace, w.Metadata.Name
	owner := map[string]string{"nube/owned-by": "Website/" + name}
	meta := map[string]any{"name": name, "namespace": ns, "labels": owner}
	objs := []any{
		map[string]any{
			"kind": "Deployment", "metadata": meta,
			"spec": map[string]any{
				"replicas": int(spec.Replicas),
				"template": map[string]any{
					"labels": map[string]string{"app": name},
					"image":  spec.Image,
					"ports":  []any{map[string]any{"containerPort": int(spec.Port)}},
					"healthCheck": map[string]any{
						"httpGet": map[string]any{"path": "/", "port": int(spec.Port)},
					},
				},
			},
		},
		map[string]any{
			"kind": "Service", "metadata": meta,
			"spec": map[string]any{"selector": map[string]string{"app": name}, "internal": spec.Host != "",
				"ports": []any{map[string]any{"port": int(spec.Port), "targetPort": int(spec.Port)}}},
		},
	}
	url := ""
	if spec.Host != "" {
		objs = append(objs, map[string]any{
			"kind": "Ingress", "metadata": meta,
			"spec": map[string]any{"rules": []any{map[string]any{
				"host":  spec.Host,
				"paths": []any{map[string]any{"path": "/", "backend": map[string]any{"service": name, "port": int(spec.Port)}}},
			}}},
		})
		url = "http://" + spec.Host + "/"
	}
	if _, err := c.ApplyObjects(ctx, objs...); err != nil {
		return err
	}

	var dep struct {
		Spec   struct{ Replicas int }
		Status struct {
			ReadyReplicas int    `json:"readyReplicas"`
			Phase         string `json:"phase"`
		}
	}
	if err := c.Get(ctx, ns, "deployments", name, &dep); err != nil {
		return err
	}
	st := websiteStatus{Phase: "Deploying", ReadyReplicas: dep.Status.ReadyReplicas, URL: url}
	if dep.Status.Phase == "Healthy" {
		st.Phase = "Running"
	} else if dep.Status.Phase == "Degraded" {
		st.Phase = "Degraded"
	}
	if err := c.UpdateStatus(ctx, ns, "websites", name, st, w.Generation); err != nil {
		return err
	}
	log.Printf("website %s/%s: %s (%d/%d ready)", ns, name, st.Phase, st.ReadyReplicas, int(spec.Replicas))
	return nil
}
