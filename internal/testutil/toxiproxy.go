// Package testutil exposes helpers used exclusively by tests in this module.
//
// Toxiproxy is a TCP proxy that can apply "toxics" (latency, bandwidth
// limit, timeout, etc.) to a named proxy. It is controlled over an HTTP
// API on a side port. This file provides a minimal, allocation-cheap
// client for the subset of operations the fault-injection interop suite
// needs: create/delete a proxy, toggle its enabled flag, and add/remove
// toxics.
package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Toxiproxy is a thin HTTP client against a running toxiproxy server.
type Toxiproxy struct {
	base string
	hc   *http.Client
}

// NewToxiproxy returns a client whose base URL points at toxiproxy's HTTP
// control port (e.g. "http://127.0.0.1:8474"). The HTTP timeout is set
// short because every API call is local and toxiproxy itself responds in
// single-digit ms; a long timeout only masks test hangs.
func NewToxiproxy(base string) *Toxiproxy {
	return &Toxiproxy{
		base: base,
		hc:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Ping returns nil if the toxiproxy control plane is reachable.
func (c *Toxiproxy) Ping() error {
	resp, err := c.hc.Get(c.base + "/version")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("toxiproxy /version: %s", resp.Status)
	}
	return nil
}

// Proxy describes one toxiproxy listener pair.
type Proxy struct {
	Name     string `json:"name"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	Enabled  bool   `json:"enabled"`
}

// CreateProxy creates a listener named `name` that forwards to `upstream`.
// listen is the TCP bind spec inside the toxiproxy container (e.g.
// "0.0.0.0:17447"); upstream is resolved in the toxiproxy container (e.g.
// "zenohd:7447"). If a proxy of the same name already exists it is
// deleted first so tests can safely re-run without a stale config.
func (c *Toxiproxy) CreateProxy(name, listen, upstream string) error {
	_ = c.DeleteProxy(name) // best-effort; first-run or cleanup paths

	body, _ := json.Marshal(Proxy{
		Name:     name,
		Listen:   listen,
		Upstream: upstream,
		Enabled:  true,
	})
	return c.postStatus(http.MethodPost, "/proxies", body, http.StatusCreated)
}

// DeleteProxy removes the named proxy and all its toxics.
func (c *Toxiproxy) DeleteProxy(name string) error {
	return c.postStatus(http.MethodDelete, "/proxies/"+name, nil, http.StatusNoContent)
}

// SetEnabled toggles the proxy's enabled flag. Disabling closes every
// active connection and refuses new ones; re-enabling restores listen.
func (c *Toxiproxy) SetEnabled(name string, enabled bool) error {
	body, _ := json.Marshal(map[string]any{"enabled": enabled})
	return c.postStatus(http.MethodPost, "/proxies/"+name, body, http.StatusOK)
}

// Toxic describes one named impairment on a proxy. Only the fields used
// by the fault interop suite are modelled; see toxiproxy's README for
// the full Attributes vocabulary.
//
//	Type     — "latency" | "bandwidth" | "timeout" | "limit_data" | ...
//	Stream   — "upstream" (client→server) | "downstream" (server→client)
//	Toxicity — 0.0..1.0; fraction of traffic affected (1.0 = always)
type Toxic struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Stream     string         `json:"stream"`
	Toxicity   float64        `json:"toxicity"`
	Attributes map[string]any `json:"attributes"`
}

// AddToxic installs a toxic. The proxy must already exist.
func (c *Toxiproxy) AddToxic(proxy string, t Toxic) error {
	body, _ := json.Marshal(t)
	return c.postStatus(http.MethodPost, "/proxies/"+proxy+"/toxics", body, http.StatusOK)
}

// RemoveToxic deletes a previously-installed toxic by name.
func (c *Toxiproxy) RemoveToxic(proxy, toxicName string) error {
	return c.postStatus(http.MethodDelete, "/proxies/"+proxy+"/toxics/"+toxicName, nil, http.StatusNoContent)
}

// Reset deletes every proxy known to toxiproxy. Useful in test teardown
// hooks to ensure state does not leak between runs.
func (c *Toxiproxy) Reset() error {
	return c.postStatus(http.MethodPost, "/reset", nil, http.StatusNoContent)
}

// postStatus issues one request and requires the response status to match.
// The response body is drained and discarded on success.
func (c *Toxiproxy) postStatus(method, path string, body []byte, wantStatus int) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("toxiproxy %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
