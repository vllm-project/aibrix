package resolver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type GatewayImage struct {
	Image      string
	Repository string
	Tag        string
}

func PrepareGatewayImage(ctx context.Context, projectRoot string, test *Test) (*GatewayImage, error) {
	if test.ProviderName() != "aibrix" {
		return nil, nil
	}
	if test.WorkspacePath == "" || test.ResolvedCommit == "" {
		return nil, nil
	}

	baseRef := strings.TrimSpace(test.Version)
	if baseRef == "" {
		baseRef = strings.TrimSpace(test.ResolvedVersion)
	}
	if baseRef == "" {
		return nil, nil
	}
	outputRepository := strings.TrimSpace(test.Gateway.Image.OutputRepository)
	if outputRepository == "" {
		return nil, fmt.Errorf("gateway.image.outputRepository is required for source-built gateway image in %s", test.Name)
	}
	baseImage := strings.TrimSpace(test.Gateway.Image.BaseImage)
	if baseImage == "" {
		return nil, fmt.Errorf("gateway.image.baseImage is required for source-built gateway image in %s", test.Name)
	}

	finalTag := baseRef + "-" + strings.TrimSpace(test.ResolvedCommit) + "-benchmark"

	builderDockerfile := filepath.Join(projectRoot, "benchmark", "testdata", "deployments", "aibrix", "gateway", "build", "Dockerfile.builder")
	benchmarkDockerfile := filepath.Join(projectRoot, "benchmark", "testdata", "deployments", "aibrix", "gateway", "build", "Dockerfile.benchmark")
	finalImage := joinImageRef(outputRepository, finalTag)
	repository, tag, parseErr := splitImageRef(finalImage)
	if parseErr != nil {
		return nil, parseErr
	}

	if err := runDocker(ctx, test.WorkspacePath, "build", "-t", "aibrix-gateway-builder", "-f", builderDockerfile, "."); err != nil {
		return nil, fmt.Errorf("failed to build gateway builder image: %w", err)
	}

	builderContainer, err := captureDocker(ctx, test.WorkspacePath, "create", "aibrix-gateway-builder")
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway builder container: %w", err)
	}
	builderContainer = strings.TrimSpace(builderContainer)
	defer func() {
		_ = runDocker(ctx, test.WorkspacePath, "rm", builderContainer)
	}()

	_ = os.Remove(filepath.Join(test.WorkspacePath, "gateway-plugins"))
	_ = os.RemoveAll(filepath.Join(test.WorkspacePath, "deps"))

	if err := runDocker(ctx, test.WorkspacePath, "cp", builderContainer+":/workspace/gateway-plugins", filepath.Join(test.WorkspacePath, "gateway-plugins")); err != nil {
		return nil, fmt.Errorf("failed to extract gateway-plugins binary: %w", err)
	}
	if err := runDocker(ctx, test.WorkspacePath, "cp", builderContainer+":/workspace/deps", filepath.Join(test.WorkspacePath, "deps")); err != nil {
		return nil, fmt.Errorf("failed to extract gateway-plugins dependencies: %w", err)
	}

	if err := runDocker(ctx, test.WorkspacePath, "manifest", "inspect", finalImage); err != nil {
		if err := runDocker(ctx, test.WorkspacePath, "build", "--build-arg", "BASE_IMAGE="+baseImage, "--build-arg", "COMMIT_HASH="+test.ResolvedCommit, "-t", finalImage, "-f", benchmarkDockerfile, "."); err != nil {
			return nil, fmt.Errorf("failed to build benchmark gateway image: %w", err)
		}
		if err := runDocker(ctx, test.WorkspacePath, "push", finalImage); err != nil {
			return nil, fmt.Errorf("failed to push benchmark gateway image: %w", err)
		}
	}

	image := &GatewayImage{
		Image:      finalImage,
		Repository: repository,
		Tag:        tag,
	}
	test.GatewayImage = image.Image
	test.GatewayImageRepository = image.Repository
	test.GatewayImageTag = image.Tag
	return image, nil
}

func joinImageRef(repository string, tag string) string {
	return fmt.Sprintf("%s:%s", strings.TrimSpace(repository), strings.TrimSpace(tag))
}

func splitImageRef(image string) (string, string, error) {
	idx := strings.LastIndex(image, ":")
	if idx <= strings.LastIndex(image, "/") {
		return "", "", fmt.Errorf("invalid image reference: %s", image)
	}
	return image[:idx], image[idx+1:], nil
}

func runDocker(ctx context.Context, cwd string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v, output: %s", err, string(output))
	}
	return nil
}

func captureDocker(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v, output: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}
