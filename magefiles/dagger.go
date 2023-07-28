//go:build mage
// +build mage

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"dagger.io/dagger"
	"github.com/containers/image/v5/docker/reference"
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

const (
	// https://hub.docker.com/_/golang/tags?page=1&name=alpine
	alpineVersion = "3.18"
	goVersion     = "1.20"

	// https://github.com/golangci/golangci-lint/releases
	golangciLintVersion = "1.53.3"

	// https://hub.docker.com/r/flyio/flyctl/tags
	flyctlVersion = "0.1.65"

	appName          = "dagger-registry-2023-01-23"
	appImageRegistry = "registry.fly.io"
	binaryName       = "registry-redirect"

	InstancesToDeploy = "3"
	// We want to avoid running multiple instances in the same region
	// If there are issues with one region, the whole service will be disrupted
	MaxInstancesPerRegion = "1"

	// https://fly.io/docs/reference/regions/#fly-io-regions
	Paris     = "cdg"
	Singapore = "sin"
	Ashburn   = "iad"

	// https://fly.io/docs/reference/configuration/#picking-a-deployment-strategy
	DeployStrategy = "rolling" // Required when MaxInstancesPerRegion set to 1
)

// golangci-lint
func Lint(ctx context.Context) {
	c := daggerClient(ctx)
	defer c.Close()

	lint(ctx, c)
}

func lint(ctx context.Context, c *dagger.Client) {
	_, err := c.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/amd64")}).
		From(fmt.Sprintf("golangci/golangci-lint:v%s-alpine", golangciLintVersion)).
		WithMountedCache("/go/pkg/mod", c.CacheVolume("gomod")).
		WithMountedDirectory("/src", sourceCode(c)).WithWorkdir("/src").
		WithExec([]string{"golangci-lint", "run", "--color", "always", "--timeout", "2m"}).
		Sync(ctx)
	if err != nil {
		panic(err)
	}
}

// go test
func Test(ctx context.Context) {
	c := daggerClient(ctx)
	defer c.Close()

	test(ctx, c)
}

func test(ctx context.Context, c *dagger.Client) {
	_, err := c.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/amd64")}).
		From(fmt.Sprintf("golang:%s-alpine%s", goVersion, alpineVersion)).
		WithMountedDirectory("/src", sourceCode(c)).WithWorkdir("/src").
		WithMountedCache("/go/pkg/mod", c.CacheVolume("gomod")).
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"go", "test", "./..."}).
		Sync(ctx)

	if err != nil {
		panic(err)
	}
}

// binary artefact used in container image
func Build(ctx context.Context) {
	c := daggerClient(ctx)
	defer c.Close()

	build(ctx, c)
}

func build(ctx context.Context, c *dagger.Client) *dagger.File {
	binaryPath := fmt.Sprintf("build/%s", binaryName)

	buildBinary, err := c.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/amd64")}).
		From(fmt.Sprintf("golang:%s-alpine%s", goVersion, alpineVersion)).
		WithMountedDirectory("/src", sourceCode(c)).WithWorkdir("/src").
		WithMountedCache("/go/pkg/mod", c.CacheVolume("gomod")).
		WithExec([]string{"go", "build", "-o", binaryPath}).
		Sync(ctx)
	if err != nil {
		panic(err)
	}

	return buildBinary.File(binaryPath)
}

func sourceCode(c *dagger.Client) *dagger.Directory {
	return c.Host().Directory(".", dagger.HostDirectoryOpts{
		Include: []string{
			"LICENSE",
			"README.md",
			"go.mod",
			"go.sum",
			"**/*.go",
		},
	})
}

// container image to private registry
func Publish(ctx context.Context) {
	c := daggerClient(ctx)
	defer c.Close()

	publish(ctx, c)
}

func publish(ctx context.Context, c *dagger.Client) string {
	binary := build(ctx, c)

	githubRef := os.Getenv("GITHUB_REF_NAME")
	if githubRef == "main" {
		return publishImage(ctx, c, binary)
	} else {
		fmt.Println("\n📦 Publishing runs only in CI, main branch")
	}

	return ""
}

// zero-downtime deploy container image
func Deploy(ctx context.Context) {
	c := daggerClient(ctx)
	defer c.Close()

	imageRef := os.Getenv("IMAGE_REF")
	if imageRef == "" {
		panic("IMAGE_REF env var must be set")
	}

	deploy(ctx, c, imageRef)
}

func deploy(ctx context.Context, c *dagger.Client, imageRef string) {
	githubRef := os.Getenv("GITHUB_REF_NAME")
	if githubRef == "main" {
		imageRefFlyValid, err := reference.ParseDockerRef(imageRef)
		if err != nil {
			panic(err)
		}

		_, err = flyctl(c).
			WithExec([]string{
				"deploy", "--now",
				"--image", imageRefFlyValid.String(),
				"--ha=false", // we will be scaling this app in the next command
				"--strategy", DeployStrategy,
			}).
			WithExec([]string{
				"scale",
				"count", InstancesToDeploy,
				"--max-per-region", MaxInstancesPerRegion,
				fmt.Sprintf("--region=%s,%s,%s", Ashburn, Paris, Singapore),
				"--yes",
			}).
			Sync(ctx)
		if err != nil {
			panic(err)
		}
	} else {
		fmt.Println("\n🎁 Deploying runs only in CI, main branch")
	}
}

// [lints, tests], builds, publishes & deploys a new version of the app
func All(ctx context.Context) {
	mg.CtxDeps(ctx, Lint, Test)

	c := daggerClient(ctx)
	defer c.Close()

	imageRef := publish(ctx, c)
	deploy(ctx, c, imageRef)
}

// stream app logs
func Logs(ctx context.Context) {
	c := daggerClient(ctx)
	defer c.Close()

	// This command does not return,
	// therefore it will never be cached,
	// and it can be run multiple times
	_, err := flyctl(c).
		WithExec([]string{"logs"}).
		Stdout(ctx)

	if err != nil {
		panic(err)
	}
}

func daggerClient(ctx context.Context) *dagger.Client {
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		panic(err)
	}
	return client
}

func publishImage(ctx context.Context, c *dagger.Client, binary *dagger.File) string {
	ref := fmt.Sprintf("%s:%s", imageName(), gitSHA())

	refWithSHA, err := c.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/amd64")}).
		From(fmt.Sprintf("alpine:%s", alpineVersion)).
		WithFile(fmt.Sprintf("/%s", binaryName), binary).
		WithNewFile("/GIT_SHA", dagger.ContainerWithNewFileOpts{
			Contents:    gitSHA(),
			Permissions: 444,
		}).
		WithNewFile("/GIT_AUTHOR", dagger.ContainerWithNewFileOpts{
			Contents:    author(),
			Permissions: 444,
		}).
		WithNewFile("/BUILD_URL", dagger.ContainerWithNewFileOpts{
			Contents:    buildURL(),
			Permissions: 444,
		}).
		WithEntrypoint([]string{fmt.Sprintf("/%s", binaryName)}).
		WithRegistryAuth(appImageRegistry, "x", flyTokenSecret(c)).
		Publish(ctx, ref)

	if err != nil {
		panic(err)
	}

	return refWithSHA
}

func gitSHA() string {
	gitSHA := os.Getenv("GITHUB_SHA")
	if gitSHA == "" {
		if gitHEAD, err := sh.Output("git", "rev-parse", "HEAD"); err == nil {
			gitSHA = fmt.Sprintf("%s.", gitHEAD)
		}
		gitSHA = fmt.Sprintf("%sdev", gitSHA)
	}

	return gitSHA
}

func author() string {
	author := os.Getenv("GITHUB_AUTHOR")
	if author == "" {
		author = os.Getenv("USER")
	}

	return author
}

func buildURL() string {
	githubServerURL := os.Getenv("GITHUB_SERVER_URL")
	githubRepository := os.Getenv("GITHUB_REPOSITORY")
	githubRunID := os.Getenv("GITHUB_RUN_ID")
	buildURL := fmt.Sprintf("%s/%s/actions/runs/%s", githubServerURL, githubRepository, githubRunID)

	if githubRunID == "" {
		if hostname, err := os.Hostname(); err == nil {
			buildURL = hostname
		}
		if cwd, err := os.Getwd(); err == nil {
			buildURL = fmt.Sprintf("%s:%s", buildURL, cwd)
		}
	}

	return buildURL
}

func flyctl(c *dagger.Client) *dagger.Container {
	c = c.Pipeline("flyctl")
	flyctl := c.Container(dagger.ContainerOpts{Platform: dagger.Platform("linux/amd64")}).Pipeline("auth").
		From(fmt.Sprintf("flyio/flyctl:v%s", flyctlVersion)).
		WithSecretVariable("FLY_API_TOKEN", flyTokenSecret(c)).
		WithEnvVariable("RUN_AT", time.Now().String()).
		WithNewFile("fly.toml", dagger.ContainerWithNewFileOpts{
			Contents: fmt.Sprintf(`
# https://fly.io/docs/reference/configuration/
app = "%s"
primary_region = "%s"

kill_signal = "SIGINT"
# Wait these many seconds for existing connections to drain before hard killing
kill_timeout = 30

[env]
  PORT = "8080"

[experimental]
  auto_rollback = true
  cmd = ["-repo", "dagger"]

[[services]]
  http_checks = []
  internal_port = 8080
  processes = ["app"]
  protocol = "tcp"
  script_checks = []
  [services.concurrency]
    hard_limit = 1000
    soft_limit = 800
    type = "connections"

  [[services.ports]]
    force_https = true
    handlers = ["http"]
    port = 80

  [[services.ports]]
    handlers = ["tls", "http"]
    port = 443

  [[services.tcp_checks]]
    grace_period = "1s"
    interval = "5s"
    restart_limit = 0
    timeout = "4s"`, appName, Ashburn)})

	return flyctl
}

func flyTokenSecret(c *dagger.Client) *dagger.Secret {
	flyToken := os.Getenv("FLY_API_TOKEN")
	if flyToken == "" {
		panic("FLY_API_TOKEN env var must be set")
	}
	return c.SetSecret("FLY_API_TOKEN", flyToken)
}

func imageName() string {
	envImageURL := os.Getenv("IMAGE_URL")
	if envImageURL != "" {
		return envImageURL
	}

	return fmt.Sprintf("%s/%s", appImageRegistry, appName)
}
