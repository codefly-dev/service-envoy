package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
)

// TestBuildContextContainsCopySources guards the build phase against the class
// of failure where the Dockerfile COPYs a file that no build step emits into
// the context — BuildKit reports it as "failed to compute cache key". It renders
// the build context the way Build does and asserts every COPY source exists.
//
// Routes are loaded first: a configured gateway always has them, and the config
// the Dockerfile bakes must not depend on runtime-only state (network mappings)
// that the build phase cannot supply.
func TestBuildContextContainsCopySources(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	service := &resources.Service{Name: "envoy", Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, filepath.Join(tmpDir, "mod", "envoy")))

	identity := &basev0.ServiceIdentity{
		Workspace:           "workspace",
		Module:              "mod",
		Name:                service.Name,
		Version:             service.Version,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: filepath.Join("mod", service.Name),
	}

	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity:     identity,
		CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	require.NoError(t, createTestRouteConfig(ctx, builder.Service, "http://127.0.0.1:9999"))
	require.NoError(t, builder.LoadRestRoutes(ctx))

	// Mirror the build-context preparation Build performs before invoking Docker.
	require.NoError(t, shared.DeleteFile(ctx, builder.Local("builder/Dockerfile")))
	require.NoError(t, builder.Templates(ctx, DockerTemplating{}, services.WithBuilder(builderFS)))

	root := builder.Location
	dockerfile, err := os.ReadFile(filepath.Join(root, "builder", "Dockerfile"))
	require.NoError(t, err)

	sources := copySources(string(dockerfile))
	require.NotEmpty(t, sources, "expected the Dockerfile to COPY at least one file")
	for _, src := range sources {
		_, err := os.Stat(filepath.Join(root, src))
		require.NoErrorf(t, err, "COPY source %q missing from build context", src)
	}

	// The baked bootstrap must be a valid Envoy config that binds the ingress
	// and admin ports, so a standalone `docker run` of the image serves them.
	bootstrap := decodeYAMLFile(t, filepath.Join(root, "builder", "envoy.yaml"))
	adminPort := nested(t, bootstrap, "admin", "address", "socket_address")["port_value"]
	require.EqualValues(t, 9901, adminPort)
	listeners := nested(t, bootstrap, "static_resources")["listeners"].([]any)
	require.Len(t, listeners, 1)
	listenerPort := nested(t, listeners[0].(map[string]any), "address", "socket_address")["port_value"]
	require.EqualValues(t, deploymentListenerPort, listenerPort)
}

// TestBuildEmitsRecipeForOutputDirectory covers the CLI-owned build path: when
// the caller sends output_directory, Build renders the recipe there and returns
// a DockerBuildPlan instead of building in-process. The plan must pass the
// CLI-side VerifyDockerBuildPlan check, and the recipe tree must be
// self-contained — every COPY source the Dockerfile names present under the
// build context — so a consumer without the codefly toolchain can buildx it.
func TestBuildEmitsRecipeForOutputDirectory(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	service := &resources.Service{Name: "envoy", Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, filepath.Join(tmpDir, "mod", "envoy")))

	identity := &basev0.ServiceIdentity{
		Workspace:           "workspace",
		Module:              "mod",
		Name:                service.Name,
		Version:             service.Version,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: filepath.Join("mod", service.Name),
	}

	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity:     identity,
		CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	outDir := t.TempDir()
	resp, err := builder.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: outDir,
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{
					DockerRepository: "registry.example.com/team",
				},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.BuildStatus_SUCCESS, resp.GetState().GetState())

	plan := resp.GetResult().GetDockerBuildPlan()
	require.NotNil(t, plan, "expected a DockerBuildPlan, not a legacy in-agent build result")
	require.Nil(t, resp.GetResult().GetDockerBuildResult(), "recipe path must not report in-agent images")

	require.NoError(t, services.VerifyDockerBuildPlan(outDir, plan), "the CLI must be able to verify the emitted plan")

	require.Len(t, plan.GetRecipes(), 1)
	recipe := plan.GetRecipes()[0]
	require.Equal(t, "builder/Dockerfile", recipe.GetDockerfile())
	require.Equal(t, ".", recipe.GetContext())
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, recipe.GetPlatforms())
	require.NotEmpty(t, recipe.GetImage())

	dockerfile, err := os.ReadFile(filepath.Join(outDir, recipe.GetDockerfile()))
	require.NoError(t, err)
	sources := copySources(string(dockerfile))
	require.NotEmpty(t, sources, "expected the Dockerfile to COPY at least one file")
	for _, src := range sources {
		_, err := os.Stat(filepath.Join(outDir, recipe.GetContext(), src))
		require.NoErrorf(t, err, "COPY source %q missing from recipe context", src)
	}
}

// TestBuildRecipeDockerfileIsValidDocker runs `docker buildx build --check`
// over the recipe Build emits into an output_directory, from that directory as
// the context — the frontend validation a consumer without the codefly
// toolchain gets for free before building. VerifyDockerBuildPlan only checks
// the tree against the digest; it does not parse the Dockerfile, so an
// instruction typo (e.g. FORM for FROM) would pass every other test and fail
// only here. The complementary risk — a COPY source absent from the emitted
// context — is caught by TestBuildEmitsRecipeForOutputDirectory. --check parses
// and lints the build graph without pulling the base or running steps, so it is
// fast and does not depend on registry access to the base image.
func TestBuildRecipeDockerfileIsValidDocker(t *testing.T) {
	requireDocker(t)

	ctx := context.Background()

	tmpDir := t.TempDir()
	service := &resources.Service{Name: "envoy", Version: "0.0.0"}
	require.NoError(t, service.SaveAtDir(ctx, filepath.Join(tmpDir, "mod", "envoy")))

	identity := &basev0.ServiceIdentity{
		Workspace:           "workspace",
		Module:              "mod",
		Name:                service.Name,
		Version:             service.Version,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: filepath.Join("mod", service.Name),
	}

	builder := NewBuilder()
	_, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity:     identity,
		CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	outDir := t.TempDir()
	resp, err := builder.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: outDir,
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{
					DockerRepository: "registry.example.com/team",
				},
			},
		},
	})
	require.NoError(t, err)
	recipe := resp.GetResult().GetDockerBuildPlan().GetRecipes()[0]

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, "docker", "buildx", "build", "--check",
		"-f", filepath.Join(outDir, filepath.FromSlash(recipe.GetDockerfile())),
		filepath.Join(outDir, filepath.FromSlash(recipe.GetContext())))
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "docker buildx build --check of the emitted recipe failed:\n%s", out)
}

// requireDocker skips a test when docker buildx is not available, so the
// recipe-validation test runs wherever a daemon exists (CI, dev with Docker)
// and is inert on machines without one rather than failing spuriously.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "buildx", "version").Run(); err != nil {
		t.Skipf("docker buildx not available: %v", err)
	}
}

// copySources returns the source argument of each COPY instruction in a
// Dockerfile, ignoring --flags and the destination.
func copySources(dockerfile string) []string {
	var sources []string
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.ToUpper(fields[0]) != "COPY" {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "--") {
				continue
			}
			sources = append(sources, field)
			break
		}
	}
	return sources
}
