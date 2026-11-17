package main

import "github.com/hivecommons/hive/pkg/config"

// defaultLLMDEndpoint is the in-cluster llm-d endpoint picker Service used when
// HIVE_LLMD_ENDPOINT is unset. The shipped Kubernetes base
// (src/deploy/k8s/deployment.yaml) sets the same value, so keep the two in
// step (llmd_endpoint_test.go).
const defaultLLMDEndpoint = config.DefaultLLMDEndpoint
