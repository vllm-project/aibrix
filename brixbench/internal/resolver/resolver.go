package resolver

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario represents a group of tests.
type Scenario struct {
	Name  string `yaml:"Scenario"`
	Tests []Test `yaml:"Tests"`
}

type Engine struct {
	Type     string `yaml:"type"`
	Manifest string `yaml:"manifest"`
}

type GatewayImageConfig struct {
	BaseImage        string `yaml:"baseImage"`
	OutputRepository string `yaml:"outputRepository"`
}

type Gateway struct {
	Env       map[string]string  `yaml:"env"`
	Resources []string           `yaml:"resources"`
	Image     GatewayImageConfig `yaml:"image"`
}

type benchmarkMetadata struct {
	Kind string `yaml:"kind"`
}

// Test defines individual test environment and benchmark configurations.
type Test struct {
	Name                   string   `yaml:"name"`
	Provider               *string  `yaml:"provider"`
	Deployer               string   `yaml:"deployer"` // Legacy alias kept for migration.
	FullStack              bool     `yaml:"fullstack"`
	VKEDev                 bool     `yaml:"vkeDev"`
	Version                string   `yaml:"version"` // e.g., v0.6.0
	Commit                 string   `yaml:"commit"`  // e.g., a1b2c3d4 (optional)
	LocalPath              string   `yaml:"localPath"`
	ControlPlane           []string `yaml:"controlplane"`
	Engine                 Engine   `yaml:"engine"`
	Benchmark              string   `yaml:"benchmark"`
	Gateway                Gateway  `yaml:"gateway"`
	WorkspacePath          string   `yaml:"-"`
	ResolvedCommit         string   `yaml:"-"`
	ResolvedVersion        string   `yaml:"-"`
	BenchmarkKind          string   `yaml:"-"`
	GatewayImage           string   `yaml:"-"`
	GatewayImageRepository string   `yaml:"-"`
	GatewayImageTag        string   `yaml:"-"`
}

func (t *Test) ProviderName() string {
	if t.Provider != nil {
		return strings.TrimSpace(*t.Provider)
	}
	return strings.TrimSpace(t.Deployer)
}

// Resolve reads a YAML file and returns a Scenario object.
func Resolve(yamlPath string) (*Scenario, error) {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var scenario Scenario
	if err := yaml.Unmarshal(data, &scenario); err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	if err := validateScenario(&scenario); err != nil {
		return nil, err
	}

	return &scenario, nil
}

func validateScenario(scenario *Scenario) error {
	for i := range scenario.Tests {
		test := &scenario.Tests[i]
		if test.VKEDev && !test.FullStack {
			return fmt.Errorf("vkeDev requires fullstack=true for %s", test.Name)
		}
		if err := validateSourceSelection(test); err != nil {
			return err
		}
		if strings.TrimSpace(test.Engine.Type) == "" {
			return fmt.Errorf("missing engine.type for %s", test.Name)
		}
		if strings.TrimSpace(test.Engine.Manifest) == "" {
			return fmt.Errorf("missing engine.manifest for %s", test.Name)
		}
		if err := populateBenchmarkMetadata(test); err != nil {
			return err
		}
	}

	return nil
}

func validateSourceSelection(test *Test) error {
	localPath := strings.TrimSpace(test.LocalPath)
	if localPath == "" {
		return nil
	}
	if test.ProviderName() != "aibrix" {
		return fmt.Errorf("localPath is only supported for provider aibrix in %s", test.Name)
	}
	if strings.TrimSpace(test.Version) != "" || strings.TrimSpace(test.Commit) != "" {
		return fmt.Errorf("localPath cannot be combined with version or commit for %s", test.Name)
	}
	return nil
}

func populateBenchmarkMetadata(test *Test) error {
	benchmarkPath := strings.TrimSpace(test.Benchmark)
	if benchmarkPath == "" {
		return fmt.Errorf("missing benchmark for %s", test.Name)
	}

	data, err := os.ReadFile(benchmarkPath)
	if err != nil {
		return fmt.Errorf("failed to read benchmark config %s for %s: %w", benchmarkPath, test.Name, err)
	}

	var metadata benchmarkMetadata
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("failed to parse benchmark config %s for %s: %w", benchmarkPath, test.Name, err)
	}

	test.BenchmarkKind = strings.TrimSpace(metadata.Kind)
	if test.BenchmarkKind == "" {
		return fmt.Errorf("missing benchmark kind in %s for %s", benchmarkPath, test.Name)
	}
	return nil
}
