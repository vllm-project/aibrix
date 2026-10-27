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
	"bytes"
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

func permissions(input io.Reader) (map[string]struct{}, error) {
	decoder := yaml.NewDecoder(input)
	var result map[string]struct{}
	for {
		var document struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Rules           yaml.Node `yaml:"rules"`
			AggregationRule any       `yaml:"aggregationRule"`
		}
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if document.Kind != "ClusterRole" {
			continue
		}
		if result != nil {
			return nil, errors.New("expected exactly one ClusterRole, found more than one")
		}
		if document.APIVersion != "rbac.authorization.k8s.io/v1" || document.Metadata.Name == "" {
			return nil, errors.New("expected a named rbac.authorization.k8s.io/v1 ClusterRole")
		}
		if document.AggregationRule != nil {
			return nil, errors.New("aggregated ClusterRoles cannot be compared without a cluster")
		}
		var rules []policyRule
		if err := document.Rules.Decode(&rules); err != nil {
			return nil, err
		}
		result = make(map[string]struct{})
		for i, rule := range rules {
			if err := addRule(result, rule); err != nil {
				return nil, fmt.Errorf("rule %d: %w", i+1, err)
			}
		}
	}
	if result == nil {
		return nil, errors.New("expected exactly one ClusterRole, found none")
	}
	return result, nil
}

func addRule(result map[string]struct{}, rule policyRule) error {
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
				result[fmt.Sprintf("url=%q verb=%q", url, verb)] = struct{}{}
			}
		}
		return nil
	}
	if len(rule.APIGroups) == 0 || len(rule.Resources) == 0 {
		return errors.New("resource rule has no API groups or resources")
	}
	// Quote explicit names so a literal "*" or "<all>" cannot mean unrestricted.
	// Wildcards stay literal; expanding them would require API discovery.
	names := []string{"<all>"}
	if len(rule.ResourceNames) != 0 {
		names = make([]string, len(rule.ResourceNames))
		for i, name := range rule.ResourceNames {
			names[i] = strconv.Quote(name)
		}
	}
	for _, group := range rule.APIGroups {
		for _, resource := range rule.Resources {
			for _, verb := range rule.Verbs {
				for _, name := range names {
					key := fmt.Sprintf("group=%q resource=%q verb=%q name=%s", group, resource, verb, name)
					result[key] = struct{}{}
				}
			}
		}
	}
	return nil
}

func difference(left, right map[string]struct{}) []string {
	var missing []string
	for p := range left {
		if _, found := right[p]; !found {
			missing = append(missing, p)
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
	config, err := os.ReadFile(os.Args[1])
	if err == nil {
		err = compare(bytes.NewReader(config), os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Controller-manager RBAC matches the rendered Helm role.")
}
