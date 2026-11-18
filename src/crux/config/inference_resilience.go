package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Inference translator resilience (hivecommons/hive#19057).
//
// The translator that carries every agent's model calls to a self-hosted
// gateway has no overall timeout (a stream can run for minutes) and does not
// retry. This block adds three opt-in timeouts and an opt-in bounded retry.
// Every default below is "off": an absent or empty `governor.inference_resilience`
// block leaves the translator byte-for-byte as it was.
const (
	// DefaultInferenceConnectTimeoutS is the default TCP connect timeout in
	// seconds. 0 = off (the transport keeps its own dial timeout).
	DefaultInferenceConnectTimeoutS = 0
	// DefaultInferenceResponseHeaderTimeoutS is the default time in seconds the
	// upstream may take to start answering. 0 = off.
	DefaultInferenceResponseHeaderTimeoutS = 0
	// DefaultInferenceIdleStreamTimeoutS is the default number of seconds a
	// response body may go without a single byte before it is cut. 0 = off.
	DefaultInferenceIdleStreamTimeoutS = 0
	// DefaultInferenceRetryEnabled is the default for the 429/5xx retry. Off.
	DefaultInferenceRetryEnabled = false
	// DefaultInferenceRetryMaxRetries is the number of retries after the first
	// attempt when retry is enabled and no value is set.
	DefaultInferenceRetryMaxRetries = 2
	// MaxInferenceRetryMaxRetries is the hard ceiling on retries; a larger
	// configured value is clamped to it.
	MaxInferenceRetryMaxRetries = 5
	// DefaultInferenceRetryBaseDelayMS is the first backoff step in
	// milliseconds; each further retry doubles it.
	DefaultInferenceRetryBaseDelayMS = 500
	// DefaultInferenceRetryMaxDelayS caps a single wait in seconds, and is the
	// longest Retry-After the translator will honor. A larger Retry-After skips
	// the retry rather than waiting less than the upstream asked for.
	DefaultInferenceRetryMaxDelayS = 20
	// MaxInferenceTimeoutS bounds any configured timeout or delay in seconds.
	MaxInferenceTimeoutS = 3600
)

// Environment overrides. An env var that holds a valid value wins over the
// yaml key; an unset or invalid one is ignored (and reported by Resolve).
const (
	EnvInferenceConnectTimeoutS        = "HIVE_INFERENCE_CONNECT_TIMEOUT_S"
	EnvInferenceResponseHeaderTimeoutS = "HIVE_INFERENCE_RESPONSE_HEADER_TIMEOUT_S"
	EnvInferenceIdleStreamTimeoutS     = "HIVE_INFERENCE_IDLE_STREAM_TIMEOUT_S"
	EnvInferenceRetryEnabled           = "HIVE_INFERENCE_RETRY_ENABLED"
	EnvInferenceRetryMaxRetries        = "HIVE_INFERENCE_RETRY_MAX_RETRIES"
	EnvInferenceRetryBaseDelayMS       = "HIVE_INFERENCE_RETRY_BASE_DELAY_MS"
	EnvInferenceRetryMaxDelayS         = "HIVE_INFERENCE_RETRY_MAX_DELAY_S"
)

// InferenceResilienceConfig is `governor.inference_resilience:`. All fields are
// optional; zero means "default", and every default is off.
type InferenceResilienceConfig struct {
	// ConnectTimeoutS bounds the TCP connect to the gateway, in seconds.
	ConnectTimeoutS int `yaml:"connect_timeout_s,omitempty" json:"connect_timeout_s,omitempty"`
	// ResponseHeaderTimeoutS bounds the wait for the upstream's response
	// headers, in seconds. A non-streaming call only gets headers once the model
	// has finished, so set it above the longest healthy generation.
	ResponseHeaderTimeoutS int `yaml:"response_header_timeout_s,omitempty" json:"response_header_timeout_s,omitempty"`
	// IdleStreamTimeoutS cuts a response body that delivers no bytes for this
	// many seconds. A slow stream that keeps producing bytes is never cut.
	IdleStreamTimeoutS int `yaml:"idle_stream_timeout_s,omitempty" json:"idle_stream_timeout_s,omitempty"`
	// RetryEnabled turns on the bounded retry for 429 and 5xx answers, which
	// only ever happens before any response byte reaches the agent.
	RetryEnabled bool `yaml:"retry_enabled,omitempty" json:"retry_enabled,omitempty"`
	// RetryMaxRetries is how many retries follow the first attempt (default 2,
	// at most 5).
	RetryMaxRetries int `yaml:"retry_max_retries,omitempty" json:"retry_max_retries,omitempty"`
	// RetryBaseDelayMS is the first backoff step in milliseconds (default 500).
	RetryBaseDelayMS int `yaml:"retry_base_delay_ms,omitempty" json:"retry_base_delay_ms,omitempty"`
	// RetryMaxDelayS caps one wait and the honored Retry-After, in seconds
	// (default 20).
	RetryMaxDelayS int `yaml:"retry_max_delay_s,omitempty" json:"retry_max_delay_s,omitempty"`
}

// InferenceResilience is the resolved, ready-to-use form of the config: plain
// durations, with the defaults and the env overrides already applied.
type InferenceResilience struct {
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	IdleStreamTimeout     time.Duration
	RetryEnabled          bool
	MaxRetries            int
	BaseDelay             time.Duration
	MaxDelay              time.Duration
}

// TimeoutsEnabled reports whether any of the three timeouts is on.
func (r InferenceResilience) TimeoutsEnabled() bool {
	return r.ConnectTimeout > 0 || r.ResponseHeaderTimeout > 0 || r.IdleStreamTimeout > 0
}

// Enabled reports whether anything differs from the unchanged translator.
func (r InferenceResilience) Enabled() bool {
	return r.TimeoutsEnabled() || r.RetryEnabled
}

// Resolve applies the defaults and the env overrides to c. A nil receiver is
// the all-off default. getenv is os.Getenv in production. The second result
// lists env vars that were set but ignored (invalid or out of range); it never
// carries their values.
func (c *InferenceResilienceConfig) Resolve(getenv func(string) string) (InferenceResilience, []string) {
	var cfg InferenceResilienceConfig
	if c != nil {
		cfg = *c
	}
	var ignored []string
	pickInt := func(env string, yamlVal int) int {
		raw := strings.TrimSpace(getenv(env))
		if raw == "" {
			return yamlVal
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			ignored = append(ignored, env)
			return yamlVal
		}
		return v
	}
	connect := clampInferenceSeconds(pickInt(EnvInferenceConnectTimeoutS, cfg.ConnectTimeoutS))
	header := clampInferenceSeconds(pickInt(EnvInferenceResponseHeaderTimeoutS, cfg.ResponseHeaderTimeoutS))
	idle := clampInferenceSeconds(pickInt(EnvInferenceIdleStreamTimeoutS, cfg.IdleStreamTimeoutS))
	retries := pickInt(EnvInferenceRetryMaxRetries, cfg.RetryMaxRetries)
	baseMS := pickInt(EnvInferenceRetryBaseDelayMS, cfg.RetryBaseDelayMS)
	maxDelayS := clampInferenceSeconds(pickInt(EnvInferenceRetryMaxDelayS, cfg.RetryMaxDelayS))

	enabled := cfg.RetryEnabled
	if raw := strings.TrimSpace(getenv(EnvInferenceRetryEnabled)); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			ignored = append(ignored, EnvInferenceRetryEnabled)
		} else {
			enabled = v
		}
	}

	if retries == 0 {
		retries = DefaultInferenceRetryMaxRetries
	}
	if retries > MaxInferenceRetryMaxRetries {
		retries = MaxInferenceRetryMaxRetries
	}
	if baseMS == 0 {
		baseMS = DefaultInferenceRetryBaseDelayMS
	}
	if maxBaseMS := MaxInferenceTimeoutS * int(time.Second/time.Millisecond); baseMS > maxBaseMS {
		baseMS = maxBaseMS
	}
	if maxDelayS == 0 {
		maxDelayS = DefaultInferenceRetryMaxDelayS
	}
	return InferenceResilience{
		ConnectTimeout:        time.Duration(connect) * time.Second,
		ResponseHeaderTimeout: time.Duration(header) * time.Second,
		IdleStreamTimeout:     time.Duration(idle) * time.Second,
		RetryEnabled:          enabled,
		MaxRetries:            retries,
		BaseDelay:             time.Duration(baseMS) * time.Millisecond,
		MaxDelay:              time.Duration(maxDelayS) * time.Second,
	}, ignored
}

func clampInferenceSeconds(v int) int {
	if v > MaxInferenceTimeoutS {
		return MaxInferenceTimeoutS
	}
	return v
}

// Summary is a log-safe one-line description (numbers only).
func (r InferenceResilience) Summary() string {
	return fmt.Sprintf("connect=%s header=%s idle=%s retry=%t max_retries=%d base=%s max_delay=%s",
		r.ConnectTimeout, r.ResponseHeaderTimeout, r.IdleStreamTimeout,
		r.RetryEnabled, r.MaxRetries, r.BaseDelay, r.MaxDelay)
}
