/*
Copyright 2026 The Parallax Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command exporter-webhook is the first-party "webhook" exporter plugin. It POSTs
// each study lifecycle event's JSON payload to a configured webhook URL.
//
// The destination URL comes from the plugin's Lifecycle config
// ({"url": "https://..."}), wired through BaseLifecycle.ConfigureFunc. Export is a
// real net/http POST of ExportRequest.PayloadJson.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-exporter-webhook).
const pluginName = "webhook"

// requestTimeout bounds a single outbound webhook POST.
const requestTimeout = 15 * time.Second

// webhookConfig is the accepted Lifecycle config block for this exporter.
type webhookConfig struct {
	URL string `json:"url"`
}

// server implements the Exporter service. It holds the configured destination URL
// (set via Lifecycle Configure) and an HTTP client.
type server struct {
	pluginv1.UnimplementedExporterServer

	mu     sync.RWMutex
	url    string
	client *http.Client
}

// configure is bound to BaseLifecycle.ConfigureFunc; it validates and stores the
// webhook URL. An empty config is accepted (URL stays unset; Export will report a
// not-configured error rather than failing the whole plugin).
func (s *server) configure(configJSON []byte) (bool, string) {
	if len(configJSON) == 0 {
		return true, ""
	}
	var cfg webhookConfig
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return false, fmt.Sprintf("exporter-webhook: invalid config: %v", err)
	}
	s.mu.Lock()
	s.url = cfg.URL
	s.mu.Unlock()
	return true, ""
}

// Export POSTs the event payload to the configured webhook URL.
func (s *server) Export(ctx context.Context, req *pluginv1.ExportRequest) (*pluginv1.ExportResponse, error) {
	s.mu.RLock()
	url := s.url
	s.mu.RUnlock()
	if url == "" {
		return &pluginv1.ExportResponse{Ok: false, Detail: "exporter-webhook: no url configured"}, nil
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.GetPayloadJson()))
	if err != nil {
		return nil, fmt.Errorf("exporter-webhook: build request for %s: %w", url, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Parallax-Event", req.GetEvent())

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return &pluginv1.ExportResponse{Ok: false, Detail: fmt.Sprintf("exporter-webhook: POST %s: %v", url, err)}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= http.StatusMultipleChoices {
		return &pluginv1.ExportResponse{Ok: false, Detail: fmt.Sprintf("exporter-webhook: POST %s returned %s", url, resp.Status)}, nil
	}
	return &pluginv1.ExportResponse{Ok: true, Detail: resp.Status}, nil
}

func main() {
	impl := &server{client: &http.Client{Timeout: requestTimeout}}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_EXPORTER,
		Lifecycle: &plugin.BaseLifecycle{
			Name:          pluginName,
			Kind:          pluginv1.PluginKind_PLUGIN_KIND_EXPORTER,
			ConfigureFunc: impl.configure,
		},
		Exporter: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
