package main

import (
	"context"
	"dagger/dagger-registry/internal/dagger"
	"fmt"
)

const (
	golangciLintVersion = "2.1-alpine@sha256:eff222d3ac17f7e2a12dbe757cb33c2dc7899cd5bfae4432594e558a1e1e0228"
	goVersion           = "1.24.4-alpine3.22@sha256:ddf52008bce1be455fe2b22d780b6693259aaf97b16383b6372f4b22dd33ad66"
	alpineVersion       = "3.22@sha256:8a1f59ffb675680d47db6337b49d22281a139e9d709335b492be023728e11715"
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
