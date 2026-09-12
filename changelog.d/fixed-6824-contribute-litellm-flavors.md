- The `/contribute` page's **openrouter**, **vllm**, **llm-d**, and **watsonx**
  options no longer generate a `just contribute-setup openrouter` (etc.) the
  Justfile rejects with `Unknown backend`: they are UI flavors of the
  `litellm` backend, and the generated host/container/k8s commands and default
  prompt now substitute `litellm` while keeping the flavor's own env exports.
