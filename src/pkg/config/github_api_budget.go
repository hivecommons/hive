package config

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAgentsGitHubAPIHourlyCap = 300
	DefaultGitHubAgentReserveFloor  = 400
)

// UnmarshalYAML accepts the operator-facing agents.github_api_hourly_cap
// scalar alongside the existing agents.<name> map entries.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	type plain Config
	node := cloneYAMLNode(value)
	if agents := mappingValueNode(&node, "agents"); agents != nil && agents.Kind == yaml.MappingNode {
		for i := 0; i < len(agents.Content)-1; i += 2 {
			if agents.Content[i].Value != "github_api_hourly_cap" {
				continue
			}
			var cap int
			if err := agents.Content[i+1].Decode(&cap); err != nil {
				return fmt.Errorf("agents.github_api_hourly_cap: %w", err)
			}
			c.AgentsGitHubAPIHourlyCap = cap
			c.agentsGitHubAPIHourlyCapSet = true
			agents.Content = append(agents.Content[:i], agents.Content[i+2:]...)
			break
		}
	}
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	cap, capSet := c.AgentsGitHubAPIHourlyCap, c.agentsGitHubAPIHourlyCapSet
	*c = Config(decoded)
	c.AgentsGitHubAPIHourlyCap = cap
	c.agentsGitHubAPIHourlyCapSet = capSet
	return nil
}

func marshalConfigYAMLWithAgentCap(c Config, out any) (any, error) {
	if !c.agentsGitHubAPIHourlyCapSet {
		return out, nil
	}
	var node yaml.Node
	if err := node.Encode(out); err != nil {
		return nil, err
	}
	root := mappingNode(&node)
	if root == nil {
		return out, nil
	}
	agents := mappingValueNode(root, "agents")
	if agents == nil {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "agents"}, &yaml.Node{Kind: yaml.MappingNode})
		agents = root.Content[len(root.Content)-1]
	}
	if agents.Kind == yaml.MappingNode {
		agents.Content = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "github_api_hourly_cap"},
			{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(c.AgentsGitHubAPIHourlyCap)},
		}, agents.Content...)
	}
	return &node, nil
}

func cloneYAMLNode(in *yaml.Node) yaml.Node {
	if in == nil {
		return yaml.Node{}
	}
	out := *in
	if len(in.Content) > 0 {
		out.Content = make([]*yaml.Node, len(in.Content))
		for i, child := range in.Content {
			clone := cloneYAMLNode(child)
			out.Content[i] = &clone
		}
	}
	return out
}

func mappingValueNode(node *yaml.Node, key string) *yaml.Node {
	node = mappingNode(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func mappingNode(node *yaml.Node) *yaml.Node {
	if node != nil && node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

func (c *Config) AgentGitHubAPIHourlyCap(agentName string) int {
	if c == nil {
		return DefaultAgentsGitHubAPIHourlyCap
	}
	if c.AgentsGitHubAPIHourlyCap == 0 && !c.agentsGitHubAPIHourlyCapSet {
		return DefaultAgentsGitHubAPIHourlyCap
	}
	if a, ok := c.Agents[agentName]; ok && a.GitHubAPIHourlyCap != nil {
		return *a.GitHubAPIHourlyCap
	}
	return c.AgentsGitHubAPIHourlyCap
}

func (c *Config) GitHubAgentReserveFloor() int {
	if c == nil || c.GitHub.AgentReserveFloor == 0 {
		return DefaultGitHubAgentReserveFloor
	}
	return c.GitHub.AgentReserveFloor
}
