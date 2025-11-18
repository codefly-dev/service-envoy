package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestRESTForwardingDocker(t *testing.T) {
	testRESTForwarding(t, resources.NewRuntimeContextContainer())
}

func TestRESTForwardingNative(t *testing.T) {
	if languages.HasGoRuntime(nil) {
		testRESTForwarding(t, resources.NewRuntimeContextNative())
	}
}

func testRESTForwarding(t *testing.T, runtimeContext *basev0.RuntimeContext) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()

	ctx := context.Background()

	// Create a mock REST backend server
	backendServer := createMockBackendServer(t)
	defer backendServer.Close()

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

	// Create a REST route configuration for forwarding
	err = createTestRouteConfig(ctx, builder.Service, backendServer.URL)
	require.NoError(t, err)

	// Reload routes
	err = builder.LoadRestRoutes(ctx)
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
	backendMapping := createBackendNetworkMapping(t, backendServer.URL)
	dependencyMappings := []*basev0.NetworkMapping{backendMapping}

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings,
		DependenciesEndpoints:  createBackendEndpoint(),
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

	// Test forwarding through envoy
	testForwarding(t, runtime, ctx, networkMappings, backendServer)
}

func createMockBackendServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	// Create a simple REST endpoint
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := map[string]interface{}{
			"message": "Hello from backend",
			"method":  r.Method,
			"path":    r.URL.Path,
		}
		json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Create a custom listener that binds to 0.0.0.0 so it's accessible from Docker
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	
	// Get the actual port
	addr := listener.Addr().(*net.TCPAddr)
	
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	server.Start()
	
	// Update the URL - use localhost for the test client, but server listens on 0.0.0.0
	server.URL = fmt.Sprintf("http://127.0.0.1:%d", addr.Port)
	t.Logf("Created mock backend server listening on 0.0.0.0:%d (accessible at %s)", addr.Port, server.URL)
	return server
}

func createTestRouteConfig(ctx context.Context, service *Service, backendURL string) error {
	// Create a REST route group configuration
	routeGroup := &RestRouteGroup{
		Module:  "mod",
		Service: "backend",
		Path:    "/api",
		Routes: []*resources.ExtendedRestRoute[Extension]{
			{
				RestRoute: resources.RestRoute{
					Path:   "/api/test",
					Method: resources.HTTPMethodGet,
				},
				Extension: Extension{
					Exposed:   true,
					Protected: false,
				},
			},
			{
				RestRoute: resources.RestRoute{
					Path:   "/api/health",
					Method: resources.HTTPMethodGet,
				},
				Extension: Extension{
					Exposed:   true,
					Protected: false,
				},
			},
		},
	}

	// Save the route configuration
	loader, err := resources.NewExtendedRestRouteLoader[Extension](ctx, service.restRoutesLocation)
	if err != nil {
		return err
	}

	err = loader.Load(ctx)
	if err != nil {
		return err
	}

	loader.AddGroup(routeGroup)
	return loader.Save(ctx)
}

func createBackendNetworkMapping(t *testing.T, backendURL string) *basev0.NetworkMapping {
	// Parse the backend URL to extract host and port
	// URL is like http://127.0.0.1:port, but server listens on 0.0.0.0
	var port uint32
	fmt.Sscanf(backendURL, "http://127.0.0.1:%d", &port)
	if port == 0 {
		// Try alternative format
		fmt.Sscanf(backendURL, "http://localhost:%d", &port)
	}
	if port == 0 {
		// Try http://0.0.0.0:port format
		fmt.Sscanf(backendURL, "http://0.0.0.0:%d", &port)
	}

	// Create endpoint with proper ApiDetails
	endpoint := createBackendEndpoint()[0]

	// Create both Container and Native instances
	// Container needs host.docker.internal to reach host from Docker
	// Native uses localhost
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

func createBackendEndpoint() []*basev0.Endpoint {
	return []*basev0.Endpoint{
		{
			Module:  "mod",
			Service: "backend",
			Name:    "rest",
			Api:     "rest",
			ApiDetails: &basev0.API{
				Value: &basev0.API_Rest{
					Rest: &basev0.RestAPI{
						Groups: []*basev0.RestRouteGroup{
							{
								Path: "/api",
								Routes: []*basev0.RestRoute{
									{
										Path:   "/test",
										Method: basev0.HTTPMethod_GET,
									},
									{
										Path:   "/health",
										Method: basev0.HTTPMethod_GET,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func testForwarding(t *testing.T, runtime *Runtime, ctx context.Context, networkMappings []*basev0.NetworkMapping, backendServer *httptest.Server) {
	// Use native network access for testing (localhost instead of host.docker.internal)
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, networkMappings, runtime.restEndpoint, resources.NewNativeNetworkAccess())
	require.NoError(t, err)

	// Wait for envoy to be ready and clusters to be initialized
	time.Sleep(5 * time.Second)
	
	// Check Envoy admin interface to verify cluster health (admin is on port 9901)
	adminURL := "http://localhost:9901/clusters"
	t.Logf("Checking Envoy admin at %s", adminURL)
	adminResp, err := http.Get(adminURL)
	if err == nil {
		defer adminResp.Body.Close()
		adminBody, _ := io.ReadAll(adminResp.Body)
		bodyStr := string(adminBody)
		// Show more of the cluster status
		if len(bodyStr) > 2000 {
			bodyStr = bodyStr[:2000] + "..."
		}
		t.Logf("Envoy clusters status:\n%s", bodyStr)
	}

	client := http.Client{Timeout: 10 * time.Second}

	// Test forwarding to /mod/backend/api/test
	t.Run("ForwardToTestEndpoint", func(t *testing.T) {
		tries := 0
		for {
			if tries > 20 {
				t.Fatal("too many tries waiting for envoy")
			}

			// Test the forwarded endpoint through envoy
			// The route should be /mod/backend/api/test
			// Use http:// prefix if not present
			url := instance.Address
			if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
				url = "http://" + url
			}
			requestURL := fmt.Sprintf("%s/mod/backend/api/test", url)
			t.Logf("Attempting request to %s (try %d)", requestURL, tries+1)
			response, err := client.Get(requestURL)
			if err != nil {
				t.Logf("Request error: %v", err)
				tries++
				time.Sleep(500 * time.Millisecond)
				continue
			}
			defer response.Body.Close()

			t.Logf("Response status: %d", response.StatusCode)
			if response.StatusCode == 503 {
				body, _ := io.ReadAll(response.Body)
				t.Logf("503 response body: %s", string(body))
				// Check Envoy admin for cluster health
				adminClient := http.Client{Timeout: 2 * time.Second}
				clustersResp, err := adminClient.Get("http://localhost:9901/clusters")
				if err == nil {
					defer clustersResp.Body.Close()
					clustersBody, _ := io.ReadAll(clustersResp.Body)
					clustersStr := string(clustersBody)
					if idx := strings.Index(clustersStr, "cluster_mod_backend"); idx != -1 {
						snippet := clustersStr[max(0, idx-100):min(len(clustersStr), idx+1000)]
						t.Logf("Cluster cluster_mod_backend status:\n%s", snippet)
					} else {
						t.Logf("Full clusters status (first 2000 chars):\n%s", clustersStr[:min(2000, len(clustersStr))])
					}
				}
				// Get upstream error stats
				statsResp, err := adminClient.Get("http://localhost:9901/stats?filter=upstream")
				if err == nil {
					defer statsResp.Body.Close()
					statsBody, _ := io.ReadAll(statsResp.Body)
					statsLines := strings.Split(string(statsBody), "\n")
					errorStats := []string{}
					for _, line := range statsLines {
						if strings.Contains(line, "503") || strings.Contains(line, "error") || strings.Contains(line, "refused") || strings.Contains(line, "timeout") || strings.Contains(line, "cluster_mod_backend") {
							errorStats = append(errorStats, line)
						}
					}
					if len(errorStats) > 0 {
						t.Logf("Envoy upstream error stats:\n%s", strings.Join(errorStats[:min(30, len(errorStats))], "\n"))
					}
				}
			}
			if response.StatusCode != http.StatusOK {
				tries++
				time.Sleep(500 * time.Millisecond)
				continue
			}

			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)

			var data map[string]interface{}
			err = json.Unmarshal(body, &data)
			require.NoError(t, err)

			require.Equal(t, "Hello from backend", data["message"])
			require.Equal(t, "GET", data["method"])
			t.Logf("Successfully forwarded request through envoy: %s", string(body))
			break
		}
	})

	// Test forwarding to /mod/backend/api/health
	t.Run("ForwardToHealthEndpoint", func(t *testing.T) {
		url := instance.Address
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "http://" + url
		}
		response, err := client.Get(fmt.Sprintf("%s/mod/backend/api/health", url))
		require.NoError(t, err)
		defer response.Body.Close()

		require.Equal(t, http.StatusOK, response.StatusCode)

		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)

		var data map[string]string
		err = json.Unmarshal(body, &data)
		require.NoError(t, err)

		require.Equal(t, "ok", data["status"])
		t.Logf("Successfully forwarded health check through envoy: %s", string(body))
	})
}

