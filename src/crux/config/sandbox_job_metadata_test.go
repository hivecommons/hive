package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSandboxJobMetadataValidation(t *testing.T) {
	tests := []struct {
		name    string
		job     SandboxJobConfig
		invalid bool
	}{
		{"valid", SandboxJobConfig{PodLabels: map[string]string{"example.com/Name": "a_B.c-9", "empty": ""}, PodAnnotations: map[string]string{"Example.COM/Note": "arbitrary text / ☃"}}, false},
		{"label key", SandboxJobConfig{PodLabels: map[string]string{"bad prefix/name": "x"}}, true},
		{"label prefix case", SandboxJobConfig{PodLabels: map[string]string{"Example.com/name": "x"}}, true},
		{"label value", SandboxJobConfig{PodLabels: map[string]string{"name": "bad value"}}, true},
		{"label value length", SandboxJobConfig{PodLabels: map[string]string{"name": strings.Repeat("a", 64)}}, true},
		{"annotation key", SandboxJobConfig{PodAnnotations: map[string]string{"bad/name/again": "x"}}, true},
		{"managed label", SandboxJobConfig{PodLabels: map[string]string{"app.kubernetes.io/managed-by": "x"}}, true},
		{"job name annotation", SandboxJobConfig{PodAnnotations: map[string]string{"job-name": "x"}}, true},
		{"reserved prefix", SandboxJobConfig{PodLabels: map[string]string{"hive.kubestellar.io/foo": "x"}}, true},
		{"reserved annotation case", SandboxJobConfig{PodAnnotations: map[string]string{"HIVE.KUBESTELLAR.IO/foo": "x"}}, true},
		{"large annotation", SandboxJobConfig{PodAnnotations: map[string]string{"a": strings.Repeat("x", 256*1024)}}, true},
		{"annotation boundary", SandboxJobConfig{PodAnnotations: map[string]string{"a": strings.Repeat("x", 256*1024-1)}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSandboxJobMetadata(&tc.job)
			if (err != nil) != tc.invalid {
				t.Fatalf("ValidateSandboxJobMetadata() error = %v, invalid = %v", err, tc.invalid)
			}
		})
	}
}

func TestSandboxJobMetadataInheritance(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{PodLabels: map[string]string{"global": "g", "shared": "g"}, PodAnnotations: map[string]string{"global": "g", "shared": "g"}}}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{PodLabels: map[string]string{"agent": "a", "shared": "a"}, PodAnnotations: map[string]string{"agent": "a", "shared": "a"}}}}
	job := agent.SandboxJob(global)
	want := map[string]string{"global": "g", "agent": "a", "shared": "a"}
	if !reflect.DeepEqual(job.PodLabels, want) || !reflect.DeepEqual(job.PodAnnotations, want) {
		t.Fatalf("merged labels = %v, annotations = %v", job.PodLabels, job.PodAnnotations)
	}
	job.PodLabels["global"] = "changed"
	job.PodAnnotations["shared"] = "changed"
	if global.Job.PodLabels["global"] != "g" || agent.Sandbox.Job.PodAnnotations["shared"] != "a" {
		t.Fatal("merged maps alias their inputs")
	}
}

func TestSandboxJobMetadataMergedLimits(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{PodLabels: map[string]string{}, PodAnnotations: map[string]string{"global": strings.Repeat("x", 128*1024)}}}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{PodLabels: map[string]string{}, PodAnnotations: map[string]string{"agent": strings.Repeat("x", 128*1024)}}}}
	if ValidateSandboxJobMetadata(global.Job) != nil || ValidateSandboxJobMetadata(agent.Sandbox.Job) != nil {
		t.Fatal("individual maps should fit")
	}
	merged := agent.SandboxJob(global)
	if err := ValidateSandboxJobMetadata(&merged); err == nil {
		t.Fatal("merged annotations exceed the byte limit")
	}
	global.Job.PodAnnotations, agent.Sandbox.Job.PodAnnotations = nil, nil
	for i := range 65 {
		global.Job.PodLabels[fmt.Sprintf("global-%d", i)] = "x"
		agent.Sandbox.Job.PodLabels[fmt.Sprintf("agent-%d", i)] = "x"
	}
	merged = agent.SandboxJob(global)
	if err := ValidateSandboxJobMetadata(&merged); err == nil {
		t.Fatal("merged labels exceed the entry limit")
	}
}
