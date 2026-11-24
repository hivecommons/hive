package clusterconfig

import (
	"errors"
	"io"
	"log/slog"
	"testing"
)

func TestDecodeChecksStartupAdmission(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	raw := `[
		{"id":"local","domain":"local.example","in_cluster":true},
		{"id":"push","domain":"push.example","pull_only":true},
		{"domain":"no-id.example","in_cluster":true},
		{"id":"remote","domain":"no-kubeconfig.example"},
		{"id":"no-domain","in_cluster":true}
	]`
	configs, err := Decode([]byte(raw), logger)
	if err != nil || len(configs) != 2 || !configs["local"].InCluster || !configs["push"].PullOnly {
		t.Fatalf("unexpected cluster admission: %+v, %v", configs, err)
	}

	if _, err := Decode([]byte(`[]`), logger); !errors.Is(err, ErrNoUsableClusters) {
		t.Fatalf("empty cluster registry should fail: %v", err)
	}
	if _, err := Decode([]byte(`[{"id":"local","domain":"local.example","in_cluster":true,"image_pull_policy":false}]`), logger); err == nil {
		t.Fatal("field types must match the startup schema")
	}
}

func TestClusterUpstreamProxyEnvironment(t *testing.T) {
	var empty *ClusterUpstreamProxy
	if empty.Enabled() || empty.EnvURL() != "" || empty.EnvHosts() != "" || empty.EnvNoProxy() != "" {
		t.Fatal("a nil proxy should emit no environment")
	}
	config := &ClusterUpstreamProxy{
		URL: " https://proxy.example ", Hosts: []string{" github.example ", " "}, NoProxy: []string{" localhost "},
	}
	if !config.Enabled() || config.EnvURL() != "https://proxy.example" ||
		config.EnvHosts() != "github.example" || config.EnvNoProxy() != "localhost" {
		t.Fatalf("unexpected proxy environment: %+v", config)
	}
}
