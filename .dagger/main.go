package main

import (
	"context"
	"dagger/dagger-registry/internal/dagger"
	"fmt"
)

const (
	golangciLintVersion = "2.1-alpine@sha256:eff222d3ac17f7e2a12dbe757cb33c2dc7899cd5bfae4432594e558a1e1e0228"
	goVersion           = "1.25.5-alpine3.23@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb"
	alpineVersion       = "3.23@sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62"
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

func (m *DaggerRegistry) Lint(ctx context.Context) *dagger.Container {
	return dag.Container().
		From(fmt.Sprintf("golangci/golangci-lint:v%s", golangciLintVersion)).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("registry-gomod")).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithDirectory("/app", m.Source).
		WithWorkdir("/app").
		WithExec([]string{"sh", "-c", "golangci-lint run --color always --timeout 2m --disable errcheck"})
}

func (m *DaggerRegistry) Test(ctx context.Context) *dagger.Container {
	return m.baseContainer(ctx).
		WithExec([]string{"sh", "-c", "go test ./..."})
}

func (m *DaggerRegistry) Build(ctx context.Context) *dagger.Container {
	binary := m.baseContainer(ctx).
		WithExec([]string{"sh", "-c", "go build -o /app/registry-redirect"}).
		File("/app/registry-redirect")

	return dag.Container().
		From("alpine:"+alpineVersion).
		WithFile("/app/registry-redirect", binary).
		WithEntrypoint([]string{"/app/registry-redirect"})
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
