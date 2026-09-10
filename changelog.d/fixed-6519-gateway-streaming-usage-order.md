- Fixed the OpenAI-to-Anthropic SSE translator applying a gateway's terminal
  usage chunk after emitting `message_delta`, so `output_tokens`/`input_tokens`
  reported to Anthropic-shaped streaming callers could reflect a stale, earlier
  usage count instead of the upstream gateway's final tally. Found and covered
  by the gateway-path canary added for #6515.
