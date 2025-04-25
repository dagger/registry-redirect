package main

import (
	"context"
	"dagger/dagger-registry/internal/dagger"
	"fmt"
)

const (
	golangciLintVersion = "2.1-alpine@sha256:eff222d3ac17f7e2a12dbe757cb33c2dc7899cd5bfae4432594e558a1e1e0228"
	goVersion           = "1.24.2-alpine3.21@sha256:7772cb5322baa875edd74705556d08f0eeca7b9c4b5367754ce3f2f00041ccee"
	alpineVersion       = "3.21@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c"
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
		WithExec([]string{"sh", "-c", "golangci-lint run --color always --timeout 2m"})
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
