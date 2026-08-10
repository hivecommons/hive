# Inference deployment manifests

This directory contains a small in-cluster inference example for Hive's `vllm`/OpenAI-compatible backend path.

## What it deploys

- `vllm-deployment.yaml` creates namespace `hive-inference`, a placeholder `hf-token` Secret, a single `vllm` Deployment, and `vllm-svc` on port 8000.
- The container is `ghcr.io/ggml-org/llama.cpp:server`, not CUDA vLLM. It serves the OpenAI-compatible API shape Hive needs, which is why it works with `backend: vllm` despite running llama.cpp.
- An init container downloads `Qwen2.5-0.5B-Instruct` GGUF into an `emptyDir` volume.
- `hive-epp-rbac.yaml` adds durable RBAC plus an `InferencePool` for the live llm-d endpoint picker service account.
- `kustomization.yaml` applies both resources.

## Prerequisites

- A Kubernetes cluster that can pull the images and has enough CPU/memory for the sample model.
- Network reachability from the Hive pod to `http://vllm-svc.hive-inference.svc:8000` or your chosen Service DNS name.
- For `hive-epp-rbac.yaml`, the InferencePool CRDs must be installed before applying that file.

## Apply

```bash
kubectl apply -k v2/deploy/inference
kubectl -n hive-inference rollout status deploy/vllm
kubectl -n hive-inference get svc vllm-svc
```

Then configure Hive:

```yaml
agents:
  guide:
    backend: vllm
    model: qwen2.5-0.5b-instruct
```

If the service DNS differs, set `HIVE_VLLM_ENDPOINT` or configure a named Model Gateway in the dashboard.

## Relationship to the `vllm` backend

`backend: vllm` does not execute a `vllm` binary. Hive launches Claude Code in bare mode and routes model traffic through its translator to the configured OpenAI-compatible endpoint. These manifests are only one possible endpoint implementation.
