package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strconv"
	"strings"
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
	"gopkg.in/yaml.v3"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

func TestCustomRoutesDocker(t *testing.T) {
	testCustomRoutes(t, resources.NewRuntimeContextContainer())
}

func TestCustomRoutesNative(t *testing.T) {
	if languages.HasGoRuntime(nil) {
		testCustomRoutes(t, resources.NewRuntimeContextNative())
	}
}

func testCustomRoutes(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)

	ctx := context.Background()

	// Create mock backend servers for admin and auth
	adminServer := createMockAdminServer(t)
	defer adminServer.Close()

	authServer := createMockAuthServer(t)
	defer authServer.Close()

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

	// Create custom route configurations
	err = createCustomRouteConfig(ctx, builder.Service, adminServer.URL, authServer.URL)
	require.NoError(t, err)

	// Reload routes
	err = builder.LoadCustomRoutes(ctx)
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

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints, runtimeContext)
	require.NoError(t, err)
	require.NotNil(t, networkMappings)
	require.Equal(t, 1, len(networkMappings))

	// Create network mappings for admin and auth backends
	adminMapping := createCustomBackendNetworkMapping(t, adminServer.URL, "admin")
	authMapping := createCustomBackendNetworkMapping(t, authServer.URL, "auth")
	dependencyMappings := []*basev0.NetworkMapping{adminMapping, authMapping}

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings,
		DependenciesEndpoints:   createBackendEndpoints("admin", "auth"),
	})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		_, _ = runtime.Stop(ctx, &runtimev0.StopRequest{})
		_, _ = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	// Start envoy
	_, err = runtime.Start(ctx, &runtimev0.StartRequest{
		DependenciesNetworkMappings: dependencyMappings,
	})
	require.NoError(t, err)

	// Test custom routes
	testCustomRouteForwarding(t, runtime, ctx, networkMappings)
}

func createMockAdminServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Admin endpoints
	mux.HandleFunc("/admin/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"users": ["admin1", "admin2"], "endpoint": "admin"}`)
	})

	mux.HandleFunc("/admin/roles", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"roles": ["admin", "user"], "endpoint": "admin"}`)
	})

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	server.Start()
	server.URL = fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
	t.Logf("Created mock admin server at: %s", server.URL)
	return server
}

func createMockAuthServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Auth endpoints
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"token": "fake-jwt-token", "endpoint": "auth"}`)
	})

	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"message": "logged out", "endpoint": "auth"}`)
	})

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	server.Start()
	server.URL = fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
	t.Logf("Created mock auth server at: %s", server.URL)
	return server
}

func createCustomRouteConfig(ctx context.Context, service *Service, adminURL, authURL string) error {
	// Ensure the customRoutesLocation directory exists
	_, err := shared.CheckDirectoryOrCreate(ctx, service.customRoutesLocation)
	if err != nil {
		return err
	}

	// Parse URLs to get ports
	var adminPort, authPort uint16
	fmt.Sscanf(adminURL, "http://127.0.0.1:%d", &adminPort)
	fmt.Sscanf(authURL, "http://127.0.0.1:%d", &authPort)

	// Create custom routes configuration
	customRoutes := []*CustomRoute{
		// Admin routes
		{
			Path:        "/admin/users",
			Method:      "GET",
			Backend:     "mod/admin",
			BackendPath: "/admin/users",
			Extension: Extension{
				Exposed:   true,
				Protected: false,
			},
		},
		{
			Path:        "/admin/roles",
			Method:      "GET",
			Backend:     "mod/admin",
			BackendPath: "/admin/roles",
			Extension: Extension{
				Exposed:   true,
				Protected: false,
			},
		},
		// Auth routes
		{
			Path:        "/auth/login",
			Method:      "POST",
			Backend:     "mod/auth",
			BackendPath: "/auth/login",
			Extension: Extension{
				Exposed:   true,
				Protected: false,
			},
		},
		{
			Path:        "/auth/logout",
			Method:      "POST",
			Backend:     "mod/auth",
			BackendPath: "/auth/logout",
			Extension: Extension{
				Exposed:   true,
				Protected: false,
			},
		},
	}

	// Save custom routes to YAML file
	routeFile := path.Join(service.customRoutesLocation, "routes.yaml")
	data, err := yaml.Marshal(customRoutes)
	if err != nil {
		return err
	}
	err = os.WriteFile(routeFile, data, 0644)
	if err != nil {
		return err
	}

	return nil
}

func createCustomBackendNetworkMapping(t *testing.T, backendURL string, serviceName string) *basev0.NetworkMapping {
	// Parse the backend URL to extract host and port
	var port uint32
	fmt.Sscanf(backendURL, "http://127.0.0.1:%d", &port)
	if port == 0 {
		fmt.Sscanf(backendURL, "http://localhost:%d", &port)
	}

	// Create endpoint
	endpoint := &basev0.Endpoint{
		Name: serviceName,
		ApiDetails: &basev0.API{
			Value: &basev0.API_Rest{
				Rest: &basev0.RestAPI{
					Groups: []*basev0.RestRouteGroup{
						{
							Path:   fmt.Sprintf("/%s", serviceName),
							Routes: []*basev0.RestRoute{},
						},
					},
				},
			},
		},
	}

	// Create both Container and Native instances
	containerInstance := resources.NewHTTPNetworkInstance("host.docker.internal", uint16(port), false)
	containerInstance.Access = resources.NewContainerNetworkAccess()

	nativeInstance := resources.NewHTTPNetworkInstance("localhost", uint16(port), false)
	nativeInstance.Access = resources.NewNativeNetworkAccess()

	return &basev0.NetworkMapping{
		Endpoint: endpoint,
		Instances: []*basev0.NetworkInstance{
			containerInstance,
			nativeInstance,
		},
	}
}

func createBackendEndpoints(serviceNames ...string) []*basev0.Endpoint {
	endpoints := []*basev0.Endpoint{}
	for _, name := range serviceNames {
		endpoints = append(endpoints, &basev0.Endpoint{
			Name: name,
			ApiDetails: &basev0.API{
				Value: &basev0.API_Rest{
					Rest: &basev0.RestAPI{
						Groups: []*basev0.RestRouteGroup{
							{
								Path:   fmt.Sprintf("/%s", name),
								Routes: []*basev0.RestRoute{},
							},
						},
					},
				},
			},
		})
	}
	return endpoints
}

func testCustomRouteForwarding(t *testing.T, runtime *Runtime, ctx context.Context, networkMappings []*basev0.NetworkMapping) {
	// Use native network access for testing
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.restEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	// Wait for envoy to be ready
	time.Sleep(5 * time.Second)

	client := http.Client{Timeout: 10 * time.Second}

	// Build base URL - instance.Address might already have http:// prefix
	baseURL := instance.Address
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}

	// Test admin routes
	t.Run("AdminUsers", func(t *testing.T) {
		url := fmt.Sprintf("%s/admin/users", baseURL)
		resp, err := client.Get(url)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		t.Logf("✅ Admin users route works")
	})

	t.Run("AdminRoles", func(t *testing.T) {
		url := fmt.Sprintf("%s/admin/roles", baseURL)
		resp, err := client.Get(url)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		t.Logf("✅ Admin roles route works")
	})

	// Test auth routes
	t.Run("AuthLogin", func(t *testing.T) {
		url := fmt.Sprintf("%s/auth/login", baseURL)
		resp, err := client.Post(url, "application/json", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		t.Logf("✅ Auth login route works")
	})

	t.Run("AuthLogout", func(t *testing.T) {
		url := fmt.Sprintf("%s/auth/logout", baseURL)
		resp, err := client.Post(url, "application/json", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		t.Logf("✅ Auth logout route works")
	})
}
