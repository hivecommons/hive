package config

import (
	"reflect"
	"testing"
)

func TestSandboxPublishVerifyPrecedence(t *testing.T) {
	global := AgentSandboxConfig{Publish: &SandboxPublishConfig{Verify: []string{"make check"}, VerifyTimeoutS: 120}}
	var none *AgentConfig
	if got := none.SandboxPublishVerify(global); !reflect.DeepEqual(got, []string{"make check"}) {
		t.Fatalf("nil agent: %q", got)
	}
	if got := none.SandboxPublishVerifyTimeoutS(global); got != 120 {
		t.Fatalf("nil agent timeout = %d", got)
	}
	plain := &AgentConfig{Sandbox: &AgentSandboxOverride{}}
	if got := plain.SandboxPublishVerify(global); !reflect.DeepEqual(got, []string{"make check"}) {
		t.Fatalf("no publish override falls through to global: %q", got)
	}
	own := &AgentConfig{Sandbox: &AgentSandboxOverride{Publish: &SandboxPublishConfig{Verify: []string{"pytest --collect-only"}, VerifyTimeoutS: 30}}}
	if got := own.SandboxPublishVerify(global); !reflect.DeepEqual(got, []string{"pytest --collect-only"}) {
		t.Fatalf("override: %q", got)
	}
	if got := own.SandboxPublishVerifyTimeoutS(global); got != 30 {
		t.Fatalf("override timeout = %d", got)
	}
	off := &AgentConfig{Sandbox: &AgentSandboxOverride{Publish: &SandboxPublishConfig{}}}
	if got := off.SandboxPublishVerify(global); len(got) != 0 {
		t.Fatalf("empty override should disable the preflight: %q", got)
	}
	if got := off.SandboxPublishVerifyTimeoutS(AgentSandboxConfig{}); got != DefaultSandboxVerifyTimeoutS {
		t.Fatalf("default timeout = %d", got)
	}
}
