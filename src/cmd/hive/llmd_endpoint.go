package main

// defaultLLMDEndpoint is the in-cluster llm-d endpoint picker Service used when
// HIVE_LLMD_ENDPOINT is unset. The shipped Kubernetes base
// (src/deploy/k8s/deployment.yaml) sets the same value, so keep the two in
// step (llmd_endpoint_test.go).
const defaultLLMDEndpoint = "http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000"
