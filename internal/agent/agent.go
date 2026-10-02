// Package agent talks to the per-node lifecycle agent. In the lab it is cmd/node-agent; on real
// nodes the same calls would be backed by the GPU Operator's driver DaemonSet, a firmware tool
// or a provisioning system.
package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/internal/k8s"
)

// Version is what a node reports about the managed component.
type Version struct {
	Current   string `json:"current"`
	Upgrading string `json:"upgrading,omitempty"` // target of an upgrade in progress
}

// Health is the telemetry checked while a node soaks after an upgrade.
type Health struct {
	XID int     `json:"xid"`
	DBE float64 `json:"dbe"`
}

// HTTP reaches the agent on each node's InternalIP (the agent uses hostNetwork).
type HTTP struct {
	Port   int
	Client *http.Client
}

// NewHTTP returns a client with timeouts.
func NewHTTP(port int) *HTTP {
	return &HTTP{Port: port, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (a *HTTP) url(n k8s.Node, path string) (string, error) {
	if n.InternalIP == "" {
		return "", fmt.Errorf("node %s has no InternalIP", n.Name)
	}
	host := n.InternalIP
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%d%s", host, a.Port, path), nil
}

func (a *HTTP) getJSON(n k8s.Node, path string, out any) error {
	u, err := a.url(n, path)
	if err != nil {
		return err
	}
	resp, err := a.Client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Version returns the installed version and any upgrade in progress.
func (a *HTTP) Version(n k8s.Node) (Version, error) {
	var v Version
	err := a.getJSON(n, "/version", &v)
	return v, err
}

// Health returns the node's GPU error state.
func (a *HTTP) Health(n k8s.Node) (Health, error) {
	var h Health
	err := a.getJSON(n, "/health", &h)
	return h, err
}

// Install asks the agent to install a version (upgrade or rollback). It returns once the request is
// accepted; progress is read with Version.
func (a *HTTP) Install(n k8s.Node, version string) error {
	u, err := a.url(n, "/install?"+url.Values{"version": {version}}.Encode())
	if err != nil {
		return err
	}
	resp, err := a.Client.Post(u, "text/plain", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
