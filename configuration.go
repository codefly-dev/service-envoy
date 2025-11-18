package main

import (
	"context"
	"embed"
	"fmt"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
)

// EnvoyConfig represents the Envoy proxy configuration structure
type EnvoyConfig struct {
	StaticResources StaticResources `yaml:"static_resources"`
	Admin           Admin           `yaml:"admin"`
}

type StaticResources struct {
	Listeners []Listener `yaml:"listeners"`
	Clusters  []Cluster  `yaml:"clusters"`
}

type Listener struct {
	Name    string       `yaml:"name"`
	Address Address      `yaml:"address"`
	FilterChains []FilterChain `yaml:"filter_chains"`
}

type SocketAddress struct {
	Address   string `yaml:"address"`
	PortValue uint16 `yaml:"port_value"`
}

type Address struct {
	SocketAddress SocketAddress `yaml:"socket_address"`
}

type FilterChain struct {
	Filters []Filter `yaml:"filters"`
}

type Filter struct {
	Name       string      `yaml:"name"`
	TypedConfig TypedConfig `yaml:"typed_config"`
}

type TypedConfig struct {
	Type            string      `yaml:"@type"`
	StatPrefix      string      `yaml:"stat_prefix"`
	CodecType       string      `yaml:"codec_type"`
	RouteConfig     RouteConfig `yaml:"route_config"`
	HTTPFilters     []HTTPFilter `yaml:"http_filters"`
	GRPCWeb         *GRPCWeb     `yaml:"grpc_web,omitempty"`
	StreamIdleTimeout string    `yaml:"stream_idle_timeout,omitempty"` // For gRPC streaming
}

type RouteConfig struct {
	Name         string       `yaml:"name"`
	VirtualHosts []VirtualHost `yaml:"virtual_hosts"`
}

type VirtualHost struct {
	Name    string   `yaml:"name"`
	Domains []string `yaml:"domains"`
	Routes  []Route  `yaml:"routes"`
	CORS    *CORS    `yaml:"cors,omitempty"` // CORS configuration for gRPC-Web
}

type CORS struct {
	AllowOriginStringMatch []map[string]string `yaml:"allow_origin_string_match,omitempty"`
	AllowMethods           string               `yaml:"allow_methods,omitempty"`
	AllowHeaders           string               `yaml:"allow_headers,omitempty"`
	MaxAge                 string               `yaml:"max_age,omitempty"`
	ExposeHeaders          string               `yaml:"expose_headers,omitempty"`
}

type Route struct {
	Match RouteMatch `yaml:"match"`
	Route RouteAction `yaml:"route"`
}

type RouteMatch struct {
	Path    string            `yaml:"path,omitempty"`
	Prefix  string            `yaml:"prefix,omitempty"`
	Headers []HeaderMatcher   `yaml:"headers,omitempty"`
}

type HeaderMatcher struct {
	Name      string     `yaml:"name"`
	StringMatch *StringMatch `yaml:"string_match,omitempty"`
	ExactMatch string    `yaml:"exact_match,omitempty"`
}

type StringMatch struct {
	Exact string `yaml:"exact,omitempty"`
	Regex string `yaml:"safe_regex,omitempty"`
}

type RouteAction struct {
	Cluster          string `yaml:"cluster"`
	PrefixRewrite    string `yaml:"prefix_rewrite,omitempty"`
	RegexRewrite     *RegexRewrite `yaml:"regex_rewrite,omitempty"`
	HostRewrite      string `yaml:"host_rewrite,omitempty"`
	AutoHostRewrite  *bool  `yaml:"auto_host_rewrite,omitempty"`
	Timeout          string `yaml:"timeout,omitempty"` // For gRPC, use "0s" to disable timeout
	MaxGrpcTimeout   string `yaml:"max_grpc_timeout,omitempty"` // Maximum gRPC timeout
}

type SafeRegex struct {
	Regex string `yaml:"safe_regex"`
}

type RegexRewrite struct {
	Pattern      *SafeRegex `yaml:"pattern"`
	Substitution string     `yaml:"substitution"`
}

type HTTPFilter struct {
	Name       string      `yaml:"name"`
	TypedConfig interface{} `yaml:"typed_config,omitempty"`
}

type GRPCWeb struct {
	Enabled bool `yaml:"enabled"`
}

type Cluster struct {
	Name            string         `yaml:"name"`
	Type            string         `yaml:"type"`
	ConnectTimeout  string         `yaml:"connect_timeout"`
	LbPolicy        string         `yaml:"lb_policy"`
	HTTP2ProtocolOptions map[string]interface{} `yaml:"http2_protocol_options,omitempty"`
	TypedExtensionProtocolOptions map[string]interface{} `yaml:"typed_extension_protocol_options,omitempty"`
	TransportSocket map[string]interface{} `yaml:"transport_socket,omitempty"`
	DnsLookupFamily string         `yaml:"dns_lookup_family,omitempty"` // V4_ONLY, V6_ONLY, AUTO
	LoadAssignment  LoadAssignment `yaml:"load_assignment"`
}

type LoadAssignment struct {
	ClusterName string   `yaml:"cluster_name"`
	Endpoints   []Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	LBEndpoints []LBEndpoint `yaml:"lb_endpoints"`
}

type LBEndpoint struct {
	Endpoint EndpointAddress `yaml:"endpoint"`
}

type EndpointAddress struct {
	Address Address `yaml:"address"`
}

type Admin struct {
	Address Address `yaml:"address"`
}

type AuthValidator struct {
	Key           string
	Configuration any
}

const JWTAuthValidatorKey = "jwt"

type JWTAuthValidator struct {
	Alg             string     `yaml:"alg"`
	Audience        []string   `yaml:"audience"`
	JwkURL          string     `yaml:"jwk_url"`
	PropagateClaims [][]string `yaml:"propagate_claims"`
	Cache           bool       `yaml:"cache"`
}

func (s *Service) writeConfig(ctx context.Context, nms []*basev0.NetworkMapping, networkAccess *basev0.NetworkAccess) error {
	return s.writeConfigWithEndpoints(ctx, nms, nil, networkAccess)
}

func (s *Service) writeConfigWithEndpoints(ctx context.Context, nms []*basev0.NetworkMapping, dependencyEndpoints []*basev0.Endpoint, networkAccess *basev0.NetworkAccess) error {
	conf, err := s.createConfigWithEndpoints(ctx, nms, dependencyEndpoints, networkAccess)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create config")
	}
	target := s.Local("routing/config/envoy.yaml")
	err = os.WriteFile(target, conf, 0o644)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot write settings to %s", target)
	}
	return nil
}

func (s *Service) createConfig(ctx context.Context, otherNetworkMappings []*basev0.NetworkMapping, networkAccess *basev0.NetworkAccess) ([]byte, error) {
	return s.createConfigWithEndpoints(ctx, otherNetworkMappings, nil, networkAccess)
}

func (s *Service) createConfigWithEndpoints(ctx context.Context, otherNetworkMappings []*basev0.NetworkMapping, dependencyEndpoints []*basev0.Endpoint, networkAccess *basev0.NetworkAccess) ([]byte, error) {
	config := EnvoyConfig{
		StaticResources: StaticResources{
			Listeners: []Listener{},
			Clusters:  []Cluster{},
		},
		Admin: Admin{
			Address: Address{
				SocketAddress: SocketAddress{
					Address:   "0.0.0.0",
					PortValue: 9901,
				},
			},
		},
	}

	// Create HTTP listener
	listener := Listener{
		Name: "listener_0",
		Address: Address{
			SocketAddress: SocketAddress{
				Address:   "0.0.0.0",
				PortValue: s.port,
			},
		},
		FilterChains: []FilterChain{},
	}

	// Build routes and clusters
	routes := []Route{}
	clusterMap := make(map[string]*Cluster)

	// Process gRPC routes FIRST to ensure they match before REST routes
	// This is important because gRPC routes use prefix matching and should take precedence
	for _, route := range s.GRPCRoutes {
		if !route.Extension.Exposed {
			continue
		}
		baseRoute := resources.UnwrapGRPCRoute(route)
		
		if dependencyEndpoints != nil {
			endpoint := resources.FindEndpointForGRPCRoute(ctx, dependencyEndpoints, baseRoute)
			if endpoint == nil {
				s.Wool.Debug("skipping gRPC route, no matching endpoint", wool.Field("route", baseRoute.Route()))
				continue
			}
			
			nm, err := resources.FindNetworkMapping(ctx, otherNetworkMappings, endpoint)
			if err != nil {
				return nil, s.Wool.Wrapf(err, "cannot find network mapping for gRPC route")
			}
			
			instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, []*basev0.NetworkMapping{nm}, endpoint, networkAccess)
	if err != nil {
				return nil, s.Wool.Wrapf(err, "cannot find network instance for gRPC route")
			}
			
			endpointPath := gatewayGRPCTarget(baseRoute)
			host, port := parseAddress(instance.Address)
			clusterName := fmt.Sprintf("cluster_grpc_%s_%s", baseRoute.Module, baseRoute.Service)
			
			// Create gRPC cluster with HTTP/2
			// According to Envoy docs, http2_protocol_options: {} enables HTTP/2 for upstream
			// Empty map is the correct way to enable HTTP/2
			if _, exists := clusterMap[clusterName]; !exists {
				// Try STRICT_DNS for gRPC clusters - more aggressive DNS caching
				// STRICT_DNS resolves DNS immediately and caches results, which can help with HTTP/2
				clusterType := "STRICT_DNS"
				clusterHost := host
				
				// If using host.docker.internal, try hardcoding to gateway IP as fallback
				// But first try STRICT_DNS with host.docker.internal
				if host == "host.docker.internal" {
					// Try STRICT_DNS first - it resolves DNS immediately and maintains connection pool
					clusterType = "STRICT_DNS"
				}
				
				cluster := &Cluster{
					Name:           clusterName,
					Type:           clusterType,
					ConnectTimeout: "10s",         // Increased timeout for gRPC
					LbPolicy:       "ROUND_ROBIN",
					DnsLookupFamily: "V4_ONLY", // Prefer IPv4 to avoid IPv6 resolution issues
					HTTP2ProtocolOptions: map[string]interface{}{}, // Empty map enables HTTP/2
					// Explicitly configure upstream HTTP protocol options via typed extension
					// This forces Envoy to use HTTP/2 for upstream connections
					TypedExtensionProtocolOptions: map[string]interface{}{
						"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": map[string]interface{}{
							"@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
							"explicit_http_config": map[string]interface{}{
								"http2_protocol_options": map[string]interface{}{},
							},
						},
					},
					LoadAssignment: LoadAssignment{
						ClusterName: clusterName,
						Endpoints: []Endpoint{
							{
								LBEndpoints: []LBEndpoint{
									{
										Endpoint: EndpointAddress{
											Address: Address{
												SocketAddress: SocketAddress{
													Address:   clusterHost, // Use host.docker.internal directly
													PortValue: port,
												},
											},
										},
									},
								},
							},
						},
					},
				}
				clusterMap[clusterName] = cluster
				s.Wool.Info("created gRPC cluster", wool.Field("name", clusterName), wool.Field("host", host), wool.Field("port", port), wool.Field("type", clusterType), wool.Field("http2_enabled", true), wool.Field("transport", "plaintext"))
			}

			// Create gRPC route
			// For gRPC, standard clients send: /package.Service/Method (e.g., /helloworld.Greeter/SayHello)
			// We match on the full gateway path and forward to the backend
			// The gateway path includes module/service prefix: /mod/backend/helloworld.Greeter/SayHello
			// But clients send: /helloworld.Greeter/SayHello
			// So we match on the service prefix to catch all methods
			servicePrefix := fmt.Sprintf("/%s.%s", baseRoute.Package, baseRoute.ServiceName) // /helloworld.Greeter
			backendPath := baseRoute.Route() // /helloworld.Greeter/SayHello
			
			// For gRPC routes, we need to match on prefix and forward without path rewrite
			// The path should be forwarded as-is to the backend
			// Set timeout to 0s to disable timeout (important for gRPC streaming)
			routes = append(routes, Route{
				Match: RouteMatch{
					Prefix: servicePrefix, // Match /helloworld.Greeter/*
				},
				Route: RouteAction{
					Cluster:        clusterName,
					Timeout:        "0s", // Disable timeout for gRPC (allows streaming)
					MaxGrpcTimeout: "0s", // Maximum gRPC timeout (0s = no limit)
					// No prefix_rewrite - forward the full path as-is
					// gRPC clients send the correct path format
				},
			})
			s.Wool.Info("gRPC route", wool.Field("match", servicePrefix), wool.Field("backend_path", backendPath), wool.Field("cluster", clusterName), wool.Field("gateway_path", endpointPath))
		}
	}

	// Process REST routes
	for _, group := range s.RestRouteGroups {
		baseGroup := resources.UnwrapRestRouteGroup(group)

		nm, err := services.NetworkInstanceForRestRouteGroup(ctx, otherNetworkMappings, baseGroup, networkAccess)
		if err != nil {
			return nil, s.Wool.Wrapf(err, "cannot get network mapping for group")
		}

		s.Wool.Debug("exposing routes", wool.Field("group", baseGroup.ServiceUnique()), wool.Field("routes", group.Routes))
		for _, route := range group.Routes {
			if !route.Extension.Exposed {
				continue
			}
			
			baseRoute := resources.UnwrapRestRoute(route)
			
			// Parse host and port from network instance address
			host, port := parseAddress(nm.Address)
			clusterName := fmt.Sprintf("cluster_%s_%s", baseGroup.Module, baseGroup.Service)
			s.Wool.Info("parsed backend address", wool.Field("address", nm.Address), wool.Field("host", host), wool.Field("port", port), wool.Field("cluster", clusterName))
			
			// Create or get cluster
			if _, exists := clusterMap[clusterName]; !exists {
				cluster := &Cluster{
					Name:           clusterName,
					Type:           "STRICT_DNS",
					ConnectTimeout: "5s",
					LbPolicy:       "ROUND_ROBIN",
					LoadAssignment: LoadAssignment{
						ClusterName: clusterName,
						Endpoints: []Endpoint{
							{
								LBEndpoints: []LBEndpoint{
									{
										Endpoint: EndpointAddress{
											Address: Address{
												SocketAddress: SocketAddress{
													Address:   host,
													PortValue: port,
												},
											},
										},
									},
								},
							},
						},
					},
				}
				// If using host.docker.internal, prefer IPv4 DNS resolution
				// This ensures we connect via IPv4 even if DNS returns IPv6 first
				if host == "host.docker.internal" {
					cluster.DnsLookupFamily = "V4_ONLY"
				}
				clusterMap[clusterName] = cluster
			}

			// Create route match - match on the full endpoint path (like KrakenD)
			// This allows proper aggregation: each route matches exactly
			endpointPath := gatewayRestTargetForRoute(baseGroup, baseRoute)
			routeMatch := RouteMatch{
				Prefix: endpointPath, // Match on full path /mod/backend/api/test
			}
			// Envoy uses headers for HTTP method matching
			if baseRoute.Method != "" {
				routeMatch.Headers = []HeaderMatcher{
					{
						Name:       ":method",
						ExactMatch: strings.ToUpper(string(baseRoute.Method)),
					},
				}
			}

			// For REST routes, we need to rewrite the path to remove the gateway prefix
			// e.g., /mod/backend/api/test -> /api/test
			// prefix_rewrite replaces the matched prefix, so matching /mod/backend/api/test
			// and rewriting to /api/test will forward /api/test to the backend
			routeAction := RouteAction{
				Cluster:       clusterName,
				PrefixRewrite: baseRoute.Path, // Replace matched prefix with backend path
			}
			s.Wool.Info("path rewrite", wool.Field("match", endpointPath), wool.Field("rewrite_to", baseRoute.Path), wool.Field("cluster", clusterName), wool.Field("method", baseRoute.Method))
			
			routes = append(routes, Route{
				Match: routeMatch,
				Route: routeAction,
			})
		}
	}

	// gRPC routes are already processed above (before REST routes)

	// Process custom routes (admin, auth, roles, etc.)
	for _, customRoute := range s.CustomRoutes {
		if !customRoute.Extension.Exposed {
				continue
		}

		// Find backend network mapping
		// Backend format: "module/service" or just "service"
		backendParts := strings.Split(customRoute.Backend, "/")
		var backendModule, backendService string
		if len(backendParts) == 2 {
			backendModule = backendParts[0]
			backendService = backendParts[1]
		} else {
			backendModule = "mod" // Default module
			backendService = backendParts[0]
		}

		// Find network mapping for this backend
		// Match by endpoint name or module/service
		var nm *basev0.NetworkMapping
		for _, mapping := range otherNetworkMappings {
			if mapping.Endpoint != nil {
				// Try matching by name first (simpler)
				if mapping.Endpoint.Name == backendService {
					nm = mapping
					s.Wool.Debug("found backend by name", wool.Field("route", customRoute.Path), wool.Field("backend", customRoute.Backend), wool.Field("endpoint_name", mapping.Endpoint.Name))
					break
				}
				// Try matching by module/service
				endpointModule := mapping.Endpoint.Module
				endpointService := mapping.Endpoint.Service
				if endpointModule == backendModule && endpointService == backendService {
					nm = mapping
					s.Wool.Debug("found backend by module/service", wool.Field("route", customRoute.Path), wool.Field("backend", customRoute.Backend))
					break
				}
			}
		}

		if nm == nil {
			s.Wool.Debug("skipping custom route, backend not found", wool.Field("route", customRoute.Path), wool.Field("backend", customRoute.Backend))
				continue
			}
			
		instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, []*basev0.NetworkMapping{nm}, nm.Endpoint, networkAccess)
			if err != nil {
			s.Wool.Debug("skipping custom route, cannot find network instance", wool.Field("route", customRoute.Path), wool.ErrField(err))
			continue
		}

		host, port := parseAddress(instance.Address)
		clusterName := fmt.Sprintf("cluster_custom_%s_%s", backendModule, backendService)

		// Create cluster if it doesn't exist
		if _, exists := clusterMap[clusterName]; !exists {
			cluster := &Cluster{
				Name:           clusterName,
				Type:           "STRICT_DNS",
				ConnectTimeout: "5s",
				LbPolicy:       "ROUND_ROBIN",
				LoadAssignment: LoadAssignment{
					ClusterName: clusterName,
					Endpoints: []Endpoint{
						{
							LBEndpoints: []LBEndpoint{
								{
									Endpoint: EndpointAddress{
										Address: Address{
											SocketAddress: SocketAddress{
												Address:   host,
												PortValue: port,
											},
										},
									},
								},
							},
						},
					},
				},
			}
			if host == "host.docker.internal" {
				cluster.DnsLookupFamily = "V4_ONLY"
			}
			clusterMap[clusterName] = cluster
		}

		// Determine backend path (use BackendPath if specified, otherwise use Path)
		backendPath := customRoute.BackendPath
		if backendPath == "" {
			backendPath = customRoute.Path
		}

		// Create route match
		routeMatch := RouteMatch{
			Prefix: customRoute.Path,
		}
		if customRoute.Method != "" {
			routeMatch.Headers = []HeaderMatcher{
				{
					Name:       ":method",
					ExactMatch: strings.ToUpper(customRoute.Method),
				},
			}
		}

		// Create route action with path rewrite
		routeAction := RouteAction{
			Cluster:       clusterName,
			PrefixRewrite: backendPath,
		}

		routes = append(routes, Route{
			Match: routeMatch,
			Route: routeAction,
		})

		s.Wool.Info("custom route", wool.Field("path", customRoute.Path), wool.Field("method", customRoute.Method), wool.Field("backend", customRoute.Backend), wool.Field("rewrite_to", backendPath))
	}

	// Add clusters to config
	for _, cluster := range clusterMap {
		endpointAddr := ""
		if len(cluster.LoadAssignment.Endpoints) > 0 && len(cluster.LoadAssignment.Endpoints[0].LBEndpoints) > 0 {
			ep := cluster.LoadAssignment.Endpoints[0].LBEndpoints[0].Endpoint.Address.SocketAddress
			endpointAddr = fmt.Sprintf("%s:%d", ep.Address, ep.PortValue)
		}
		s.Wool.Info("adding cluster", wool.Field("name", cluster.Name), wool.Field("type", cluster.Type), wool.Field("endpoints", len(cluster.LoadAssignment.Endpoints)), wool.Field("endpoint_address", endpointAddr))
		config.StaticResources.Clusters = append(config.StaticResources.Clusters, *cluster)
	}

	// Create HTTP connection manager filter
	// Router filter must be last
	httpFilters := []HTTPFilter{
		{
			Name: "envoy.filters.http.router",
			TypedConfig: map[string]string{
				"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
			},
		},
	}

		// Add CORS filter if REST routes are present (before router)
		// CORS filter in Envoy v3 - minimal configuration, CORS is handled via route-level configuration
		if len(s.RestRouteGroups) > 0 {
			httpFilters = append([]HTTPFilter{
				{
					Name: "envoy.filters.http.cors",
					TypedConfig: map[string]interface{}{
						"@type": "type.googleapis.com/envoy.extensions.filters.http.cors.v3.Cors",
					},
				},
			}, httpFilters...)
		}

	// Add gRPC-Web and gRPC stats filters if gRPC routes are present (before router)
	// gRPC-Web enables browser-based gRPC clients
	// gRPC stats helps Envoy properly handle gRPC requests
	if len(s.GRPCRoutes) > 0 {
		httpFilters = append([]HTTPFilter{
			{
				Name: "envoy.filters.http.cors",
				TypedConfig: map[string]interface{}{
					"@type": "type.googleapis.com/envoy.extensions.filters.http.cors.v3.Cors",
				},
			},
			{
				Name: "envoy.filters.http.grpc_web",
				TypedConfig: map[string]interface{}{
					"@type": "type.googleapis.com/envoy.extensions.filters.http.grpc_web.v3.GrpcWeb",
				},
			},
			{
				Name: "envoy.filters.http.grpc_stats",
				TypedConfig: map[string]interface{}{
					"@type": "type.googleapis.com/envoy.extensions.filters.http.grpc_stats.v3.FilterConfig",
				},
			},
		}, httpFilters...)
	}

	filterChain := FilterChain{
		Filters: []Filter{
			{
				Name: "envoy.filters.network.http_connection_manager",
				TypedConfig: TypedConfig{
					Type:       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
					StatPrefix: "ingress_http",
					CodecType:  "AUTO", // AUTO allows HTTP/1.1 and HTTP/2, which is needed for gRPC
					StreamIdleTimeout: "0s", // Disable stream idle timeout for gRPC (allows long-lived streams)
					RouteConfig: RouteConfig{
						Name: "local_route",
					VirtualHosts: []VirtualHost{
						{
							Name:    "backend",
							Domains: []string{"*"},
							Routes:  routes,
							// Add CORS configuration for gRPC-Web support
							CORS: &CORS{
								AllowOriginStringMatch: []map[string]string{{"exact": "*"}},
								AllowMethods:           "GET, PUT, DELETE, POST, OPTIONS",
								AllowHeaders:           "keep-alive,user-agent,cache-control,content-type,content-transfer-encoding,custom-header-1,x-accept-content-transfer-encoding,x-accept-response-streaming,x-user-agent,x-grpc-web,grpc-timeout",
								MaxAge:                 "1728000", // 20 days
								ExposeHeaders:          "custom-header-1,grpc-status,grpc-message",
							},
						},
					},
					},
					HTTPFilters: httpFilters,
				},
			},
		},
	}

	listener.FilterChains = []FilterChain{filterChain}
	config.StaticResources.Listeners = []Listener{listener}

	// Marshal to YAML
	content, err := yaml.Marshal(&config)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "cannot marshal Envoy config")
	}

	return content, nil
}

func parseAddress(address string) (string, uint16) {
	// Remove http:// or https:// prefix
	address = strings.TrimPrefix(address, "http://")
	address = strings.TrimPrefix(address, "https://")
	
	// Split host:port
	parts := strings.Split(address, ":")
	if len(parts) != 2 {
		return "localhost", 8080
	}

	var port uint16
	fmt.Sscanf(parts[1], "%d", &port)
	host := parts[0]
	
	// Keep host.docker.internal as-is - Envoy will handle DNS resolution
	// We configure the cluster to use V4_ONLY DNS lookup to prefer IPv4
	
	return host, port
}

func gatewayRestTarget(r *resources.RestRouteGroup) string {
	return fmt.Sprintf("/%s/%s%s", r.Module, r.Service, r.Path)
}

func gatewayRestTargetForRoute(r *resources.RestRouteGroup, route *resources.RestRoute) string {
	// Each route gets its own endpoint path
	return fmt.Sprintf("/%s/%s%s", r.Module, r.Service, route.Path)
}

func gatewayGRPCTarget(r *resources.GRPCRoute) string {
	// gRPC endpoint format: /mod/service/package.Service/Method
	return fmt.Sprintf("/%s/%s%s", r.Module, r.Service, r.Route())
}

//go:embed templates/envoy.yaml
var config embed.FS
