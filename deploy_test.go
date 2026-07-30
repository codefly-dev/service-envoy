package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// deployToDir runs the builder Deploy pipeline for the given namespace against a
// fresh temp destination and returns it. It fails the test if Deploy errors —
// which includes the manifest failing Core's static conformance contract.
func deployToDir(t *testing.T, namespace string) string {
	t.Helper()
	ctx := context.Background()

	identity := &resources.ServiceIdentity{Workspace: "workspace", Module: "mod", Name: "envoy", Version: "0.0.0"}
	base := &services.Base{
		Wool:                 wool.Get(ctx),
		Identity:             identity,
		Information:          &services.Information{Service: resources.ToServiceWithCase(identity), Module: resources.ToModuleWithCase(identity)},
		EnvironmentVariables: resources.NewEnvironmentVariableManager(),
	}
	base.Builder = &services.BuilderWrapper{Base: base}
	base.SetDockerImage(resources.NewDockerImage("envoyproxy/envoy:v1.38.0"))

	builder := &Builder{Service: &Service{Base: base, Settings: &Settings{}}}

	destination := t.TempDir()
	req := &builderv0.DeploymentRequest{
		Environment: &basev0.Environment{Name: "test"},
		Deployment: &builderv0.Deployment{
			Kind: &builderv0.Deployment_Kubernetes{
				Kubernetes: &builderv0.KubernetesDeployment{
					Namespace:   namespace,
					Destination: destination,
					Profile:     builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
				},
			},
		},
	}

	resp, err := builder.Deploy(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	return destination
}

func decodeYAMLFile(t *testing.T, path string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, yaml.Unmarshal(content, &out))
	return out
}

// TestDeployRendersRunnableEnvoyManifests guards the wiring that lets the
// rendered pod actually start Envoy: the container must launch Envoy against
// the mounted config, the mount path must match the launch argument, and the
// generated bootstrap must bind the ingress listener to the Service port (not
// the zero port the builder used to emit) plus the admin port the probes hit.
func TestDeployRendersRunnableEnvoyManifests(t *testing.T) {
	const configPath = "/codefly/config/envoy.yaml"
	destination := deployToDir(t, "codefly-test")

	deployment := decodeYAMLFile(t, filepath.Join(destination, "base", "deployment.yaml"))
	containers := nested(t, deployment, "spec", "template", "spec")["containers"].([]any)
	require.Len(t, containers, 1)
	container := containers[0].(map[string]any)

	command := toStringSlice(container["command"])
	require.Equal(t, []string{"envoy", "-c", configPath}, command,
		"container must launch Envoy against the mounted config")

	var mountPaths []string
	for _, raw := range container["volumeMounts"].([]any) {
		mountPaths = append(mountPaths, raw.(map[string]any)["mountPath"].(string))
	}
	require.Contains(t, mountPaths, configPath,
		"the config launch argument must correspond to an actual volumeMount")

	// The probes target the admin port; assert the bootstrap actually serves
	// there, and that the ingress listener binds the Service port.
	configMap := decodeYAMLFile(t, filepath.Join(destination, "overlays", "test", "configmap.yaml"))
	settings := nested(t, configMap, "data")["settings"].(string)
	var envoyConfig map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(settings), &envoyConfig))

	adminPort := nested(t, envoyConfig, "admin", "address", "socket_address")["port_value"]
	require.EqualValues(t, 9901, adminPort)

	listeners := nested(t, envoyConfig, "static_resources")["listeners"].([]any)
	require.Len(t, listeners, 1)
	listenerPort := nested(t, listeners[0].(map[string]any), "address", "socket_address")["port_value"]
	require.EqualValues(t, deploymentListenerPort, listenerPort,
		"ingress listener must bind the Service port, not the zero port")
}

// TestDeployQuotesNumericNamespace ensures an all-numeric namespace survives as
// a string. An unquoted template value would decode as a YAML integer, which
// drops metadata.namespace and fails Core's static conformance — so Deploy
// erroring here (via deployToDir's require.NoError) is the regression signal.
func TestDeployQuotesNumericNamespace(t *testing.T) {
	destination := deployToDir(t, "12345")

	deployment := decodeYAMLFile(t, filepath.Join(destination, "base", "deployment.yaml"))
	namespace := nested(t, deployment, "metadata")["namespace"]
	require.Equal(t, "12345", namespace)
}

func nested(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	current := m
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		require.Truef(t, ok, "expected map at key %q", key)
		current = next
	}
	return current
}

func toStringSlice(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}
