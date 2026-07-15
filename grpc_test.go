package main

// NOTE: Envoy has native gRPC support! No Enterprise Edition needed!
// This test verifies that Envoy correctly forwards gRPC requests

import (
	"context"
	"fmt"
	"net"
	"path"
	"strconv"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"

	testpb "github.com/codefly-dev/service-envoy/testdata/testpb"
)

// testGreeterServer implements the Greeter service
type testGreeterServer struct {
	testpb.UnimplementedGreeterServer
}

func (s *testGreeterServer) SayHello(ctx context.Context, req *testpb.HelloRequest) (*testpb.HelloResponse, error) {
	return &testpb.HelloResponse{
		Message: fmt.Sprintf("Hello %s from backend!", req.Name),
	}, nil
}

func TestGRPCForwardingDocker(t *testing.T) {
	testGRPCForwarding(t, resources.NewRuntimeContextContainer())
}

func TestGRPCForwardingNative(t *testing.T) {
	if languages.HasGoRuntime(nil) {
		testGRPCForwarding(t, resources.NewRuntimeContextNative())
	}
}

func testGRPCForwarding(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)

	ctx := context.Background()

	// Create a mock gRPC backend server
	backendServer, backendPort := createMockGRPCBackend(t)
	defer backendServer.Stop()

	var err error
	tmpDir := t.TempDir()

	workspace := &resources.Workspace{Name: "test"}
	service := &resources.Service{Name: "envoy", Version: "0.0.0"}
	err = service.SaveAtDir(ctx, path.Join(tmpDir, fmt.Sprintf("mod/%s", service.Name)))
	require.NoError(t, err)
	service.WithModule("mod")
	mod := &resources.Module{Name: "mod"}

	err = mod.SaveToDir(ctx, path.Join(tmpDir, "mod"))
	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}
	env := resources.LocalEnvironment()

	// randomize
	env.NamingScope = strconv.Itoa(time.Now().Second())

	// Create builder and set up envoy service
	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{
		Identity:     identity,
		CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Create a gRPC route configuration for forwarding
	err = createTestGRPCRouteConfig(ctx, builder.Service, backendPort)
	require.NoError(t, err)

	// Reload routes (now that directory exists)
	err = builder.LoadGRPCRoutes(ctx)
	require.NoError(t, err)

	// Now run envoy runtime
	runtime := NewRuntime()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true,
	})
	require.NoError(t, err)

	require.Equal(t, 1, len(runtime.Endpoints))

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints)
	require.NoError(t, err)
	require.NotNil(t, networkMappings)
	require.Equal(t, 1, len(networkMappings))

	// Create network mapping for the backend service
	backendMapping := createBackendGRPCNetworkMapping(t, backendPort)
	dependencyMappings := []*basev0.NetworkMapping{backendMapping}

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings,
		DependenciesEndpoints:   createBackendGRPCEndpoint(),
	})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		if runtime.runner != nil {
			_ = runtime.runner.Stop(ctx)
			_ = runtime.runner.Shutdown(ctx)
		}
		_, _ = runtime.Stop(ctx, &runtimev0.StopRequest{})
		_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	// Start envoy
	_, err = runtime.Start(ctx, &runtimev0.StartRequest{
		DependenciesNetworkMappings: dependencyMappings,
	})
	require.NoError(t, err)

	// Test forwarding through envoy
	testGRPCForwardingConnection(t, runtime, ctx, networkMappings, backendPort)
}

func createMockGRPCBackend(t *testing.T) (*grpc.Server, uint16) {
	// Create a simple gRPC server
	// Bind to 0.0.0.0 explicitly to ensure it's accessible from Docker containers
	lis, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)

	port := uint16(lis.Addr().(*net.TCPAddr).Port)

	server := grpc.NewServer()

	// Register the test gRPC service
	greeter := &testGreeterServer{}
	testpb.RegisterGreeterServer(server, greeter)

	// Enable reflection so clients can discover services
	reflection.Register(server)

	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("gRPC server error: %v", err)
		}
	}()

	// Wait a moment for the server to start listening
	time.Sleep(100 * time.Millisecond)

	// Verify the server is listening
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 1*time.Second)
	if err != nil {
		t.Logf("Warning: Backend server might not be ready: %v", err)
	} else {
		conn.Close()
	}

	t.Logf("Created mock gRPC backend server on port: %d with Greeter service", port)
	return server, port
}

func createTestGRPCRouteConfig(ctx context.Context, service *Service, backendPort uint16) error {
	// Create the directory first
	_, err := shared.CheckDirectoryOrCreate(ctx, service.grpcRoutesLocation)
	if err != nil {
		return err
	}

	// Create a gRPC route configuration
	route := &resources.ExtendedGRPCRoute[Extension]{
		GRPCRoute: resources.GRPCRoute{
			Name:        "SayHello",
			Package:     "helloworld",
			ServiceName: "Greeter",
			Module:      "mod",
			Service:     "backend",
		},
		Extension: Extension{
			Exposed:   true,
			Protected: false,
		},
	}

	// Save the route configuration
	loader, err := resources.NewExtendedGRPCRouteLoader[Extension](ctx, service.grpcRoutesLocation)
	if err != nil {
		return err
	}

	loader.Add(route)
	return loader.Save(ctx)
}

func createBackendGRPCNetworkMapping(t *testing.T, backendPort uint16) *basev0.NetworkMapping {
	// Create endpoint with proper ApiDetails
	endpoint := createBackendGRPCEndpoint()[0]

	// Create both Container and Native instances
	containerInstance := resources.NewNetworkInstance("host.docker.internal", backendPort)
	containerInstance.Access = resources.NewContainerNetworkAccess()

	nativeInstance := resources.NewNetworkInstance("localhost", backendPort)
	nativeInstance.Access = resources.NewNativeNetworkAccess()

	return &basev0.NetworkMapping{
		Endpoint: endpoint,
		Instances: []*basev0.NetworkInstance{
			containerInstance,
			nativeInstance,
		},
	}
}

func createBackendGRPCEndpoint() []*basev0.Endpoint {
	return []*basev0.Endpoint{
		{
			Module:  "mod",
			Service: "backend",
			Name:    "grpc",
			Api:     "grpc",
			ApiDetails: &basev0.API{
				Value: &basev0.API_Grpc{
					Grpc: &basev0.GrpcAPI{
						Rpcs: []*basev0.RPC{
							{
								Name:        "SayHello",
								ServiceName: "Greeter",
							},
						},
						Package: "helloworld",
					},
				},
			},
		},
	}
}

func testGRPCForwardingConnection(t *testing.T, runtime *Runtime, ctx context.Context, networkMappings []*basev0.NetworkMapping, backendPort uint16) {
	// Use native network access for testing
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.restEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	// Wait for Envoy to be ready and backend to be accessible
	time.Sleep(7 * time.Second)

	// Extract port from instance address
	var port uint16
	fmt.Sscanf(instance.Address, "http://localhost:%d", &port)
	if port == 0 {
		fmt.Sscanf(instance.Address, "localhost:%d", &port)
	}

	// Make gRPC call through Envoy
	conn, err := grpc.NewClient(fmt.Sprintf("localhost:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err, "Failed to connect to Envoy")
	defer conn.Close()

	client := testpb.NewGreeterClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.SayHello(ctx, &testpb.HelloRequest{Name: "World"})
	require.NoError(t, err, "gRPC call through Envoy should succeed")
	require.Equal(t, "Hello World from backend!", resp.Message, "Response message should match expected value")

	t.Logf("✅ Successfully made gRPC call through Envoy: %s", resp.Message)
}
