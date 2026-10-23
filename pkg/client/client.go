// Package client is a small Go SDK for the Nube API, for operators and other
// programs. Inside a pod it configures itself from the environment:
// NUBE_API_SERVER and NUBE_CA_HASH are injected by the node agent, and
// NUBE_TOKEN comes from a Secret (create an edit token for the operator).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/api"
	"github.com/thisisnabi/Nube/internal/pki"
)

// Re-exported types operators work with.
type (
	CustomObject = api.CustomObject
	WatchEvent   = api.WatchEvent
)

type Client struct {
	Server string
	Token  string
	http   *http.Client
}

// New creates a client. caHash pins the cluster CA ("sha256:..."); it may be
// empty for plain-HTTP clusters.
func New(server, token, caHash string) *Client {
	tr := &http.Transport{}
	if strings.HasPrefix(server, "https://") {
		tr.TLSClientConfig = pki.ClientConfig(caHash, "", caHash == "")
	}
	return &Client{Server: strings.TrimRight(server, "/"), Token: token, http: &http.Client{Transport: tr}}
}

// FromEnv uses NUBE_API_SERVER (or NUBE_SERVER), NUBE_TOKEN and NUBE_CA_HASH.
func FromEnv() (*Client, error) {
	server := os.Getenv("NUBE_API_SERVER")
	if server == "" {
		server = os.Getenv("NUBE_SERVER")
	}
	if server == "" || os.Getenv("NUBE_TOKEN") == "" {
		return nil, errors.New("set NUBE_API_SERVER and NUBE_TOKEN")
	}
	return New(server, os.Getenv("NUBE_TOKEN"), os.Getenv("NUBE_CA_HASH")), nil
}

// APIError is returned for non-2xx answers.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("nube API %d: %s", e.Status, e.Message) }

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 404
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, ctype string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(resp.Body)
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Apply creates or updates the objects in a YAML or JSON manifest.
func (c *Client) Apply(ctx context.Context, manifest string) ([]string, error) {
	var out struct {
		Results []string `json:"results"`
	}
	err := c.do(ctx, "POST", "/api/v1/apply", strings.NewReader(manifest), "application/yaml", &out)
	return out.Results, err
}

// ApplyObjects marshals objects (maps or structs with kind/metadata/spec) and applies them.
func (c *Client) ApplyObjects(ctx context.Context, objs ...any) ([]string, error) {
	b, err := json.Marshal(objs)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []string `json:"results"`
	}
	err = c.do(ctx, "POST", "/api/v1/apply", bytes.NewReader(b), "application/json", &out)
	return out.Results, err
}

// Get fetches one object into out. plural is e.g. "deployments" or "websites".
func (c *Client) Get(ctx context.Context, namespace, plural, name string, out any) error {
	return c.do(ctx, "GET", "/api/v1/namespaces/"+url.PathEscape(namespace)+"/"+plural+"/"+url.PathEscape(name), nil, "", out)
}

// List returns the raw items of a kind ("" namespace = all you may see).
func (c *Client) List(ctx context.Context, namespace, plural string) ([]json.RawMessage, error) {
	path := "/api/v1/" + plural
	if namespace != "" {
		path = "/api/v1/namespaces/" + url.PathEscape(namespace) + "/" + plural
	}
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	err := c.do(ctx, "GET", path, nil, "", &out)
	return out.Items, err
}

// Delete removes one object.
func (c *Client) Delete(ctx context.Context, namespace, plural, name string) error {
	return c.do(ctx, "DELETE", "/api/v1/namespaces/"+url.PathEscape(namespace)+"/"+plural+"/"+url.PathEscape(name), nil, "", nil)
}

// UpdateStatus sets the status of a custom object and records the spec
// generation it reflects.
func (c *Client) UpdateStatus(ctx context.Context, namespace, plural, name string, status any, observedGeneration int64) error {
	b, err := json.Marshal(map[string]any{"status": status, "observedGeneration": observedGeneration})
	if err != nil {
		return err
	}
	return c.do(ctx, "PUT", "/api/v1/namespaces/"+url.PathEscape(namespace)+"/"+plural+"/"+url.PathEscape(name)+"/status",
		bytes.NewReader(b), "application/json", nil)
}

// Watch long-polls changes of a custom resource after a revision. It returns
// the events and the revision to pass next time (0 = start: lists everything).
func (c *Client) Watch(ctx context.Context, plural string, since int64) ([]WatchEvent, int64, error) {
	var out struct {
		Revision int64        `json:"revision"`
		Events   []WatchEvent `json:"events"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/api/v1/watch/%s?since=%d&timeout=30", plural, since), nil, "", &out)
	if err != nil {
		return nil, since, err
	}
	next := out.Revision
	for _, e := range out.Events {
		if e.Revision > next {
			next = e.Revision
		}
	}
	return out.Events, next, nil
}

// Run is a minimal operator loop: it calls reconcile for every changed
// object of a custom kind (and for all of them every resync), retrying on
// errors. deleted is true for removed objects.
func (c *Client) Run(ctx context.Context, plural string, resync time.Duration, reconcile func(ctx context.Context, obj *CustomObject, deleted bool) error) error {
	var since int64
	lastFull := time.Time{}
	for ctx.Err() == nil {
		from := since
		if time.Since(lastFull) > resync {
			from, lastFull = 0, time.Now()
		}
		events, next, err := c.Watch(ctx, plural, from)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			time.Sleep(3 * time.Second)
			continue
		}
		since = max(since, next)
		for _, e := range events {
			b, _ := json.Marshal(e.Object)
			var obj CustomObject
			json.Unmarshal(b, &obj)
			if err := reconcile(ctx, &obj, e.Type == "DELETED"); err != nil {
				fmt.Fprintf(os.Stderr, "reconcile %s/%s: %v\n", obj.Metadata.Namespace, obj.Metadata.Name, err)
				lastFull = time.Time{} // retry everything on the next round
			}
		}
	}
	return ctx.Err()
}
