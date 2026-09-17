- Moved inference route and gateway resolution out of `cmd/hive`'s `main.go`
  into a new `pkg/inference`
  ([#7238](https://github.com/hivecommons/hive/issues/7238) stage 3): the
  LiteLLM endpoint/model route, watsonx gateway selection, per-gateway bearer
  and header resolution, and supervision of the optional bundled local LiteLLM
  proxy. Behaviour is unchanged. The bundled proxy's loopback port is now
  covered by a test asserting it cannot collide with the inference translator's
  port — previously only a comment said so, in a file that no longer sits
  beside either constant.
