/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command verify-rbac compares the generated controller role with Helm output.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type policyRule struct {
	APIGroups       []string       `yaml:"apiGroups"`
	Resources       []string       `yaml:"resources"`
	Verbs           []string       `yaml:"verbs"`
	ResourceNames   []string       `yaml:"resourceNames"`
	NonResourceURLs []string       `yaml:"nonResourceURLs"`
	Unknown         map[string]any `yaml:",inline"`
}

type clusterRole struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules           []policyRule `yaml:"rules"`
	AggregationRule any          `yaml:"aggregationRule"`
}

// A tuple preserves name restrictions and distinguishes resource permissions
// from non-resource URL permissions. Wildcards remain literal: comparing them
// with individual resources would require a cluster's API discovery data.
type permission struct {
	group, resource, verb, name, url string
	allNames                         bool
}

func (p permission) String() string {
	if p.url != "" {
		return fmt.Sprintf("url=%q verb=%q", p.url, p.verb)
	}
	name := "<all>"
	if !p.allNames {
		name = strconv.Quote(p.name)
	}
	return fmt.Sprintf("group=%q resource=%q verb=%q name=%s", p.group, p.resource, p.verb, name)
}

func readRole(input io.Reader) (*clusterRole, error) {
	decoder := yaml.NewDecoder(input)
	var role *clusterRole
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		var header struct{ Kind string }
		if err := document.Decode(&header); err != nil {
			return nil, err
		}
		if header.Kind != "ClusterRole" {
			continue
		}
		if role != nil {
			return nil, errors.New("expected exactly one ClusterRole, found more than one")
		}
		role = &clusterRole{}
		if err := document.Decode(role); err != nil {
			return nil, err
		}
	}
	if role == nil {
		return nil, errors.New("expected exactly one ClusterRole, found none")
	}
	if role.APIVersion != "rbac.authorization.k8s.io/v1" || role.Metadata.Name == "" {
		return nil, errors.New("expected a named rbac.authorization.k8s.io/v1 ClusterRole")
	}
	if role.AggregationRule != nil {
		return nil, errors.New("aggregated ClusterRoles cannot be compared without a cluster")
	}
	return role, nil
}

func addRule(result map[permission]struct{}, rule policyRule) error {
	if len(rule.Unknown) != 0 || len(rule.Verbs) == 0 {
		return errors.New("rule has unknown fields or no verbs")
	}
	if len(rule.NonResourceURLs) != 0 {
		if len(rule.APIGroups)+len(rule.Resources)+len(rule.ResourceNames) != 0 {
			return errors.New("rule mixes resource and non-resource permissions")
		}
		for _, url := range rule.NonResourceURLs {
			if url == "" {
				return errors.New("rule has an empty non-resource URL")
			}
			for _, verb := range rule.Verbs {
				result[permission{url: url, verb: verb}] = struct{}{}
			}
		}
		return nil
	}
	if len(rule.APIGroups) == 0 || len(rule.Resources) == 0 {
		return errors.New("resource rule has no API groups or resources")
	}
	names := rule.ResourceNames
	if len(names) == 0 {
		names = []string{""}
	}
	for _, group := range rule.APIGroups {
		for _, resource := range rule.Resources {
			for _, verb := range rule.Verbs {
				for _, name := range names {
					key := permission{group: group, resource: resource, verb: verb, name: name}
					key.allNames = len(rule.ResourceNames) == 0
					result[key] = struct{}{}
				}
			}
		}
	}
	return nil
}

func permissions(input io.Reader) (map[permission]struct{}, error) {
	role, err := readRole(input)
	if err != nil {
		return nil, err
	}
	result := make(map[permission]struct{})
	for i, rule := range role.Rules {
		if err := addRule(result, rule); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
	}
	return result, nil
}

func difference(left, right map[permission]struct{}) []string {
	var missing []string
	for p := range left {
		if _, found := right[p]; !found {
			missing = append(missing, p.String())
		}
	}
	slices.Sort(missing)
	return missing
}

func compare(config, rendered io.Reader) error {
	left, err := permissions(config)
	if err != nil {
		return fmt.Errorf("config RBAC: %w", err)
	}
	right, err := permissions(rendered)
	if err != nil {
		return fmt.Errorf("rendered Helm RBAC: %w", err)
	}
	missing, extra := difference(left, right), difference(right, left)
	if len(missing)+len(extra) != 0 {
		return fmt.Errorf("RBAC rules differ\nonly in config:\n%s\nonly in Helm:\n%s",
			strings.Join(missing, "\n"), strings.Join(extra, "\n"))
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: verify-rbac CONFIG_ROLE < HELM_RENDERED_YAML")
		os.Exit(2)
	}
	config, err := os.Open(os.Args[1])
	if err == nil {
		err = compare(config, os.Stdin)
		if closeErr := config.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Controller-manager RBAC matches the rendered Helm role.")
}
