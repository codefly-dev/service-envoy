package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
