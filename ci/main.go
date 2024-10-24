package main

import (
	"context"
	"dagger/ci/internal/dagger"
	"fmt"
)

const (
	golangciLintVersion = "1.60.3"
	goVersion           = "1.23"
	alpineVersion       = "3.20"
)

type Ci struct {
	Source *dagger.Directory
}

func New(
	// +defaultPath="../"
	source *dagger.Directory,
) *Ci {
	return &Ci{
		Source: source,
	}
}

func (m *Ci) Lint(ctx context.Context) *dagger.Container {
	return dag.Container().
		From(fmt.Sprintf("golangci/golangci-lint:v%s-alpine", golangciLintVersion)).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("registry-gomod")).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithDirectory("/app", m.Source).
		WithWorkdir("/app").
		WithExec([]string{"sh", "-c", "golangci-lint run --color always --timeout 2m"})
}

func (m *Ci) Test(ctx context.Context) *dagger.Container {
	return m.baseContainer(ctx).
		WithExec([]string{"sh", "-c", "go test ./..."})
}

func (m *Ci) Build(ctx context.Context) *dagger.Container {
	binary := m.baseContainer(ctx).
		WithExec([]string{"sh", "-c", "go build -o /app/registry-redirect"}).
		File("/app/registry-redirect")

	return dag.Container().
		From("alpine:"+alpineVersion).
		WithFile("/app/registry-redirect", binary).
		WithEntrypoint([]string{"/app/registry-redirect"})
}

func (m *Ci) baseContainer(ctx context.Context) *dagger.Container {
	return dag.Container().
		From(fmt.Sprintf("golang:%s-alpine%s", goVersion, alpineVersion)).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("registry-go-build")).
		WithEnvVariable("GOCACHE", "/root/.cache/go-build").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("registry-gomod")).
		WithEnvVariable("GOMODCACHE", "/go/pkg/mod").
		WithDirectory("/app", m.Source).
		WithWorkdir("/app")
}
