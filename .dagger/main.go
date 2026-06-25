package main

import (
	"context"
	"dagger/dagger-registry/internal/dagger"
	"fmt"
)

const (
	golangciLintVersion = "2.12.2-alpine@sha256:91b27804074a0bacea298707f016911e60cf0cdbc6c7bf5ccacb5f0606d18d60"
	goVersion           = "1.25.5-alpine3.23@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb"
	alpineVersion       = "3.23@sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62"
	flyctlVersion       = "0.1.78"

	appName          = "dagger-registry-2023-01-23"
	appImageRegistry = "registry.fly.io"
	binaryName       = "registry-redirect"
)

type DaggerRegistry struct {
	Source *dagger.Directory
}

func New(
	// +defaultPath="./"
	source *dagger.Directory,
) *DaggerRegistry {
	return &DaggerRegistry{
		Source: source,
	}
}

// +check
func (m *DaggerRegistry) Lint(ctx context.Context) (string, error) {
	return dag.Container().
		From(fmt.Sprintf("golangci/golangci-lint:v%s", golangciLintVersion)).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("registry-gomod")).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithDirectory("/app", m.Source).
		WithWorkdir("/app").
		WithExec([]string{"sh", "-c", "golangci-lint run --color always --timeout 2m --disable errcheck"}).
		Stdout(ctx)
}

// +check
func (m *DaggerRegistry) Test(ctx context.Context) (string, error) {
	return m.baseContainer(ctx).
		WithExec([]string{"sh", "-c", "go test ./..."}).
		Stdout(ctx)
}

func (m *DaggerRegistry) Build(ctx context.Context) *dagger.Container {
	binary := m.baseContainer(ctx).
		WithExec([]string{"sh", "-c", fmt.Sprintf("go build -o /app/%s", binaryName)}).
		File(fmt.Sprintf("/app/%s", binaryName))

	return dag.Container().
		From("alpine:"+alpineVersion).
		WithFile(fmt.Sprintf("/app/%s", binaryName), binary).
		WithEntrypoint([]string{fmt.Sprintf("/app/%s", binaryName)})
}

func (m *DaggerRegistry) Publish(
	ctx context.Context,
	flyToken *dagger.Secret,
	// +optional
	gitSha string,
	// +optional
	gitAuthor string,
	// +optional
	buildURL string,
	// +optional
	imageURL string,
) (string, error) {
	return m.image(ctx, gitSha, gitAuthor, buildURL).
		WithRegistryAuth(appImageRegistry, "x", flyToken).
		Publish(ctx, fmt.Sprintf("%s:%s", imageName(imageURL), imageTag(gitSha)))
}

func (m *DaggerRegistry) baseContainer(ctx context.Context) *dagger.Container {
	return dag.Container().
		From(fmt.Sprintf("golang:%s", goVersion)).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("registry-go-build")).
		WithEnvVariable("GOCACHE", "/root/.cache/go-build").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("registry-gomod")).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithDirectory("/app", m.Source).
		WithWorkdir("/app")
}

func (m *DaggerRegistry) image(ctx context.Context, gitSha, gitAuthor, buildURL string) *dagger.Container {
	return m.Build(ctx).
		WithNewFile("/GIT_SHA", imageTag(gitSha), dagger.ContainerWithNewFileOpts{Permissions: 0o444}).
		WithNewFile("/GIT_AUTHOR", valueOrDefault(gitAuthor, "unknown"), dagger.ContainerWithNewFileOpts{Permissions: 0o444}).
		WithNewFile("/BUILD_URL", valueOrDefault(buildURL, "unknown"), dagger.ContainerWithNewFileOpts{Permissions: 0o444}).
		WithDirectory("/src", m.Source)
}

func imageName(imageURL string) string {
	if imageURL != "" {
		return imageURL
	}

	return fmt.Sprintf("%s/%s", appImageRegistry, appName)
}

func imageTag(gitSha string) string {
	return valueOrDefault(gitSha, "dev")
}

func valueOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
