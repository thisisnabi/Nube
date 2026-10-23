// Package cli implements the kubectl-style command line client.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisnabi/Nube/internal/pki"
)

type clientConfig struct {
	Server   string `json:"server"`
	Token    string `json:"token"`
	CAHash   string `json:"caHash,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
}

func configPath() string {
	if p := os.Getenv("NUBE_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".nube", "config.json")
}

func loadConfig(flags map[string]string) clientConfig {
	var c clientConfig
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	if v := os.Getenv("NUBE_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("NUBE_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("NUBE_CA_HASH"); v != "" {
		c.CAHash = v
	}
	if v := flags["server"]; v != "" {
		c.Server = v
	}
	if v := flags["token"]; v != "" {
		c.Token = v
	}
	if v := flags["ca-hash"]; v != "" {
		c.CAHash = v
	}
	if flags["insecure-skip-tls-verify"] != "" || os.Getenv("NUBE_INSECURE") == "true" {
		c.Insecure = true
	}
	// Convenience when running on a server itself.
	if c.Token == "" {
		if b, err := os.ReadFile("/var/lib/nube/token"); err == nil {
			c.Token = strings.TrimSpace(string(b))
		}
	}
	if c.CAHash == "" {
		if b, err := os.ReadFile("/var/lib/nube/ca.hash"); err == nil {
			c.CAHash = strings.TrimSpace(string(b))
		}
	}
	if c.Server == "" {
		c.Server = "https://127.0.0.1:7070"
		if c.CAHash == "" {
			c.Server = "http://127.0.0.1:7070"
		}
	}
	c.Server = strings.TrimRight(c.Server, "/")
	return c
}

func saveConfig(c clientConfig) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

type client struct {
	cfg  clientConfig
	http *http.Client
}

func newClient(flags map[string]string) *client {
	c := &client{cfg: loadConfig(flags)}
	c.rebuild(30 * time.Second)
	return c
}

func (c *client) rebuild(timeout time.Duration) {
	tr := &http.Transport{TLSClientConfig: pki.ClientConfig(c.cfg.CAHash, "", c.cfg.Insecure)}
	c.http = &http.Client{Timeout: timeout, Transport: tr}
}

func (c *client) request(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if strings.HasPrefix(c.cfg.Server, "https://") && c.cfg.CAHash == "" && !c.cfg.Insecure {
		return nil, errors.New("the server uses HTTPS but no CA is configured: run `nube login --server " + c.cfg.Server + " --token TOKEN`")
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		var rd io.Reader
		if body != nil {
			if b, ok := body.(*bytes.Reader); ok {
				b.Seek(0, io.SeekStart) // rewind for retries
			}
			rd = body
		}
		req, err := http.NewRequest(method, c.cfg.Server+path, rd)
		if err != nil {
			return nil, err
		}
		if c.cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cannot reach %s: %v (set --server or run `nube login`)", c.cfg.Server, err)
		}
		// 503 during HA leader election: retry briefly.
		if resp.StatusCode == http.StatusServiceUnavailable && attempt < 3 {
			lastErr = decodeErr(resp)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, decodeErr(resp)
		}
		return resp, nil
	}
	return nil, lastErr
}

func decodeErr(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		if resp.StatusCode == 401 {
			return errors.New(e.Error + " (run `nube login --server URL --token TOKEN`)")
		}
		return errors.New(e.Error)
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (c *client) getJSON(path string, out any) error {
	resp, err := c.request("GET", path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) send(method, path string, body []byte, contentType string, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	resp, err := c.request(method, path, rd, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) postJSON(path string, in, out any) error {
	b, _ := json.Marshal(in)
	return c.send("POST", path, b, "application/json", out)
}
