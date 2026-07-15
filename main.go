package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
	"os"
	"path"
	"strings"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(info)))

var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("routing"),
)

type Settings struct {
}

var runtimeImage = &resources.DockerImage{
	Name:   "envoyproxy/envoy",
	Tag:    "v1.30.11",
	Digest: "sha256:b5cc70f5fe5503858817e897ae1da5d873dc32cbc493790b4e330b8a42c4af9d",
}

type Extension struct {
	Exposed   bool `yaml:"exposed"`
	Protected bool `yaml:"protected"`
}

// RestRoute extends the concept of RestRoute to add API Gateway concepts
type RestRoute = resources.ExtendedRestRoute[Extension]

// RestRouteGroup extends the concept of RestRouteGroup to add API Gateway concepts
type RestRouteGroup = resources.ExtendedRestRouteGroup[Extension]

type Service struct {
	*services.Base

	// Access
	port uint16

	restRoutesLocation   string
	grpcRoutesLocation   string
	customRoutesLocation string

	RestRouteGroups []*RestRouteGroup
	GRPCRoutes      []*resources.ExtendedGRPCRoute[Extension]
	CustomRoutes    []*CustomRoute

	// Auth
	requiresAuth bool
	validators   []*AuthValidator

	// Settings
	*Settings

	restEndpoint *basev0.Endpoint
}

func (s *Service) Setup(ctx context.Context) error {
	s.restRoutesLocation = s.Local("routing/rest")
	s.grpcRoutesLocation = s.Local("routing/grpc")
	s.customRoutesLocation = s.Local("routing/custom")
	return nil
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	rm, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readme), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Docker: true,
		},
		Protocols: []agentv0.Protocol_Type{
			agentv0.Protocol_HTTP,
			agentv0.Protocol_GRPC,
		},
		ReadMe: rm,
	}.Build(), nil
}

func NewService() *Service {
	// Use Background context for service initialization as this is called at startup
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

// LoadRestRoutes from routing configuration folder
func (s *Service) LoadRestRoutes(ctx context.Context) error {
	loader, err := resources.NewExtendedRestRouteLoader[Extension](ctx, s.restRoutesLocation)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create route loader")
	}
	err = loader.Load(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load routes")
	}
	s.RestRouteGroups = loader.Groups()
	s.Wool.Debug("known REST route groups", wool.SliceCountField(s.RestRouteGroups))
	// Check if we have protected routes
	for _, group := range s.RestRouteGroups {
		for _, route := range group.Routes {
			if route.Extension.Protected {
				s.requiresAuth = true
			}
		}
	}
	return nil
}

// LoadGRPCRoutes from routing configuration folder
func (s *Service) LoadGRPCRoutes(ctx context.Context) error {
	loader, err := resources.NewExtendedGRPCRouteLoader[Extension](ctx, s.grpcRoutesLocation)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create gRPC route loader")
	}
	err = loader.Load(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load gRPC routes")
	}
	s.GRPCRoutes = loader.All()
	s.Wool.Debug("known gRPC routes", wool.SliceCountField(s.GRPCRoutes))
	return nil
}

// CustomRoute defines a custom route for admin, auth, roles, etc.
type CustomRoute struct {
	Path        string    `yaml:"path"`         // e.g., "/admin/users" or "/auth/login"
	Method      string    `yaml:"method"`       // GET, POST, PUT, DELETE, etc.
	Backend     string    `yaml:"backend"`      // Backend service identifier (module/service)
	BackendPath string    `yaml:"backend_path"` // Optional: path to forward to backend (defaults to Path)
	Extension   Extension `yaml:"extension"`
}

// LoadCustomRoutes loads custom routes from routing/custom directory
func (s *Service) LoadCustomRoutes(ctx context.Context) error {
	// Check if directory exists
	exists, err := shared.DirectoryExists(ctx, s.customRoutesLocation)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot check custom routes directory")
	}
	if !exists {
		s.Wool.Debug("custom routes directory does not exist, skipping")
		return nil
	}

	// Load YAML files from custom routes directory
	entries, err := os.ReadDir(s.customRoutesLocation)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot read custom routes directory")
	}

	s.CustomRoutes = []*CustomRoute{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		filePath := path.Join(s.customRoutesLocation, name)
		data, err := os.ReadFile(filePath)
		if err != nil {
			s.Wool.Warn("cannot read custom routes file", wool.Field("file", filePath), wool.ErrField(err))
			continue
		}

		var routes []*CustomRoute
		err = yaml.Unmarshal(data, &routes)
		if err != nil {
			s.Wool.Warn("cannot parse custom routes file", wool.Field("file", filePath), wool.ErrField(err))
			continue
		}
		s.CustomRoutes = append(s.CustomRoutes, routes...)
	}

	s.Wool.Debug("loaded custom routes", wool.Field("count", len(s.CustomRoutes)))
	return nil
}

type ValidatorConfiguration struct {
	Jwt *struct {
		Audience string `yaml:"audience"`
		URL      string `yaml:"url"`
	} `yaml:"jwt"`
}

func (s *Service) CreateValidators(ctx context.Context, confs ...*basev0.Configuration) ([]*AuthValidator, error) {
	var auths []*AuthValidator
	for _, conf := range confs {
		auth, err := resources.GetConfigurationInformation(ctx, conf, "auth")
		if err != nil || auth == nil {
			continue
		}
		// Get the Type of the auth configuration
		var vc ValidatorConfiguration
		err = configurations.InformationUnmarshal(auth, &vc)
		if err != nil {
			return nil, s.Wool.Wrapf(err, "cannot unmarshal auth configuration")
		}
		if vc.Jwt != nil {
			jwtConf := JWTAuthValidator{
				Alg:             "RS256",
				Audience:        []string{vc.Jwt.Audience},
				JwkURL:          fmt.Sprintf("%s/.well-known/jwks.json", vc.Jwt.URL),
				PropagateClaims: [][]string{{"sub", wool.Header(wool.UserAuthIDKey)}},
				Cache:           true,
			}
			auths = append(auths,
				&AuthValidator{
					Key:           JWTAuthValidatorKey,
					Configuration: jwtConf},
			)
		}
	}
	if len(auths) == 0 {
		return nil, s.Wool.NewError("no auth configuration found")
	}
	return auths, nil

}

func main() {
	agents.Serve(agents.PluginRegistration{
		Agent:   NewService(),
		Runtime: NewRuntime(),
		Builder: NewBuilder(),
	})
}

//go:embed agent.codefly.yaml
var info embed.FS

//go:embed templates/agent
var readme embed.FS
