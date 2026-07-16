package main

import (
	"context"
	"embed"
	"fmt"
	"github.com/codefly-dev/core/agents/communicate"
	dockerhelpers "github.com/codefly-dev/core/agents/helpers/docker"
	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/templates"
	"github.com/codefly-dev/core/wool"
)

type ImportRoute struct {
	*resources.RestRoute
	service string
	module  string
}

type ImportGRPC struct {
	*resources.GRPCRoute
}

func (g *ImportGRPC) Unique() string {
	return fmt.Sprintf("%s%s %s", g.Module, g.Service, g.Name)

}

func (imp *ImportRoute) Unique() string {
	return fmt.Sprintf("%s%s %s", resources.ServiceUnique(imp.module, imp.service), imp.RestRoute.Path, imp.RestRoute.Method)
}

type Builder struct {
	services.BuilderServer
	*Service

	syncForREST []*ImportRoute
	answers     map[string]*agentv0.Answer
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return nil, err
	}

	requirements.Localize(s.Location)

	err = s.Setup(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	if req.CreationMode != nil {
		s.Wool.Info("in creation mode")
		s.Builder.CreationMode = req.CreationMode

		_, err = shared.CheckDirectoryOrCreate(ctx, s.restRoutesLocation)
		if err != nil {
			return s.Builder.LoadError(err)
		}

		s.Builder.GettingStarted, err = templates.ApplyTemplateFrom(ctx, shared.Embed(factoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return nil, err
		}

		return s.Builder.LoadResponse()
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	err = s.LoadRestRoutes(ctx)
	if err != nil {
		return s.Builder.LoadError(err)
	}

	err = s.LoadGRPCRoutes(ctx)
	if err != nil {
		// gRPC routes are optional
		s.Wool.Debug("no gRPC routes found or error loading", wool.ErrField(err))
	}

	err = s.LoadCustomRoutes(ctx)
	if err != nil {
		// Custom routes are optional
		s.Wool.Debug("no custom routes found or error loading", wool.ErrField(err))
	}

	if req.SyncMode != nil {
		s.Builder.SyncMode = req.SyncMode
	}

	return s.Builder.LoadResponse()
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.DependencyEndpoints = req.DependenciesEndpoints

	if s.Builder.SyncMode != nil {
		s.Wool.Debug("in sync mode")
		err := s.UpdateAvailableRoutesForSync(ctx)

		if err != nil {
			return s.Builder.InitError(err)
		}
	}
	//
	//hash, err := requirements.Hash(ctx)
	//if err != nil {
	//	return s.Builder.InitError(err)
	//}

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) UnknownRestRoutes(ctx context.Context) ([]*resources.RestRouteGroup, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("examining REST routes from dependency endpoints", wool.SliceCountField(s.DependencyEndpoints))
	// supported routes should correspond to dependency endpoints
	var updatedGroups []*RestRouteGroup
	for _, group := range s.RestRouteGroups {
		baseGroup := resources.UnwrapRestRouteGroup(group)
		matchingEndpoint := resources.FindEndpointForRestRoute(ctx, s.DependencyEndpoints, baseGroup)
		if matchingEndpoint != nil {
			updatedGroups = append(updatedGroups, group)
			continue
		}
		err := baseGroup.Delete(ctx, s.restRoutesLocation)
		if err != nil {
			return nil, s.Wool.Wrapf(err, "cannot delete group")
		}
	}

	var known []*resources.RestRouteGroup
	for _, group := range updatedGroups {
		known = append(known, resources.UnwrapRestRouteGroup(group))
	}
	s.Wool.Debug("known route groups", wool.SliceCountField(known))

	return resources.DetectNewRoutesFromEndpoints(ctx, s.DependencyEndpoints, known)
}

func (s *Builder) UpdateAvailableRoutesForSync(ctx context.Context) error {
	defer s.Wool.Catch()

	newRestRoutes, err := s.UnknownRestRoutes(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot compute unknown REST routes")
	}
	s.Wool.Debug("unknown REST groups", wool.SliceCountField(newRestRoutes))

	s.syncForREST = []*ImportRoute{}
	for _, group := range newRestRoutes {
		for _, route := range group.Routes {
			// Create the extended route
			imp := &ImportRoute{RestRoute: route, service: group.Service, module: group.Module}
			s.Wool.Debug("module", wool.Field("module", exposeRestWithoutAuth(imp)))
			s.syncForREST = append(s.syncForREST, imp)
		}
	}

	if len(s.syncForREST) == 0 {
		return nil
	}
	s.Wool.Debug("found new routes", wool.SliceCountField(s.syncForREST))

	return nil
}

func exposeRestWithAuth(imp *ImportRoute) string {
	return fmt.Sprintf("expose-rest-with-auth-%s", imp.Unique())
}
func exposeRestWithoutAuth(imp *ImportRoute) string {
	return fmt.Sprintf("expose-rest-without-auth-%s", imp.Unique())
}
func hiddenRest(imp *ImportRoute) string {
	return fmt.Sprintf("hidden-rest-%s", imp.Unique())
}

func (s *Builder) syncQuestions() []*agentv0.Question {
	var questions []*agentv0.Question
	if len(s.syncForREST) > 0 {
		s.Wool.Info("Detected new REST routes! Let's do some import")
	}
	for _, imp := range s.syncForREST {
		s.Wool.Debug("new route", wool.Field("route", imp.Unique()))
		questions = append(questions,
			communicate.NewChoice(&agentv0.Message{Name: imp.Unique(),
				Message:     fmt.Sprintf("Want to expose REST route: %s %s for service <%s> from module <%s>", imp.Path, imp.Method, imp.service, imp.module),
				Description: fmt.Sprintf("Corresponding route on the API service will be /%s/%s%s", imp.module, imp.service, imp.Path)},
				&agentv0.Message{Name: exposeRestWithAuth(imp), Message: "Yes (authenticated)"},
				&agentv0.Message{Name: exposeRestWithoutAuth(imp), Message: "Yes (non authenticated)"},
				&agentv0.Message{Name: hiddenRest(imp), Message: "No (internal only)"}),
		)
	}

	return questions
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if len(s.syncForREST) == 0 {
		return s.Builder.SyncResponse()
	}

	restRouteLoader, err := resources.NewExtendedRestRouteLoader[Extension](ctx, s.restRoutesLocation)
	if err != nil {
		return s.Builder.SyncError(err)
	}

	err = restRouteLoader.Load(ctx)
	if err != nil {
		return s.Builder.SyncError(err)
	}

	for _, imp := range s.syncForREST {
		choice, err := communicate.Choice(s.answers, imp.Unique())
		if err != nil {
			return s.Builder.SyncError(err)
		}
		exposeOption := ""
		if choice != nil {
			exposeOption = choice.Option
		}
		group := restRouteLoader.GroupFor(resources.ServiceUnique(imp.module, imp.service), imp.Path)
		if group == nil {
			group = &RestRouteGroup{Module: imp.module, Service: imp.service, Path: imp.Path}
			restRouteLoader.AddGroup(group)
		}
		route := RestRoute{RestRoute: *imp.RestRoute}
		if exposeOption == hiddenRest(imp) {
			route.Extension.Exposed = false
		} else {
			s.Wool.Debug("exposing", wool.Field("key", exposeOption))
			route.Extension.Exposed = true
			if exposeOption == exposeRestWithAuth(imp) {
				route.Extension.Protected = true
			}
		}
		group.Add(route)
	}
	err = restRouteLoader.Save(ctx)
	if err != nil {
		return s.Builder.SyncError(err)
	}

	// Get all the routes
	err = s.LoadRestRoutes(ctx)
	if err != nil {
		return s.Builder.SyncError(err)
	}

	// Create the configuration

	return s.Builder.SyncResponse()
}

// Envoy's generated image only adds configuration to this digest-pinned base,
// so the base inventory is authoritative for executable packages.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.AuditContainer(ctx, req, runtimeImage.FullName())
}

func (s *Builder) SBOM(ctx context.Context, _ *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SBOMContainer(ctx, runtimeImage.FullName())
}

type Env struct {
	Key   string
	Value string
}

type DockerTemplating struct {
	Envs []Env
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	dockerRequest, err := s.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return nil, s.Wool.Wrapf(err, "can only do docker build request")
	}

	image := s.DockerImage(dockerRequest)

	s.Wool.Debug("building docker runtimeImage", wool.Field("runtimeImage", image.FullName()))
	if !dockerhelpers.IsValidDockerImageName(image.Name) {
		return s.Builder.BuildError(fmt.Errorf("invalid docker runtimeImage name: %s", image.Name))
	}

	docker := DockerTemplating{}

	err = shared.DeleteFile(ctx, s.Local("builder/Dockerfile"))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	err = s.Templates(ctx, docker, services.WithBuilder(builderFS))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	builder, err := dockerhelpers.NewBuilder(dockerhelpers.BuilderConfiguration{
		Root:        s.Location,
		Dockerfile:  "builder/Dockerfile",
		Destination: image,
		Output:      s.Wool,
	})
	if err != nil {
		return s.Builder.BuildError(err)
	}
	_, err = builder.Build(ctx)
	if err != nil {
		return s.Builder.BuildError(err)
	}
	s.Builder.WithDockerImages(image)

	return s.Builder.BuildResponse()

}

type LoadBalancer struct {
	Enabled bool
	Host    string
}

type Parameters struct {
	LoadBalancer
	Configuration string
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Inputs: services.DeploymentInputs{
			OwnEndpoints:             true,
			OwnConfiguration:         true,
			DependencyConfigurations: true,
		},
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			configuration, err := s.createConfig(ctx, req.GetDependenciesNetworkMappings(), resources.NewContainerNetworkAccess())
			if err != nil {
				return s.Wool.Wrapf(err, "cannot write config")
			}
			deployment.Parameters = Parameters{
				LoadBalancer:  LoadBalancer{},
				Configuration: string(configuration),
			}
			return nil
		},
	})
}

/* Creation */

func (s *Builder) Options() []*agentv0.Question {
	return nil
}

// Communicate drives the interactive Q&A stream. In sync mode it asks the
// per-route "expose?" choices computed during Init; otherwise it runs the
// (currently empty) create-mode Options. Answers are stored on the builder for
// Sync/Create to read back.
func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	var questions []*agentv0.Question
	if len(s.syncForREST) > 0 {
		questions = s.syncQuestions()
	} else {
		questions = s.Options()
	}
	answers, err := asker.RunSequence(questions)
	if err != nil {
		return err
	}
	s.answers = answers
	return nil
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Templates(ctx, s.Information, services.WithFactory(factoryFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoint(ctx)
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.Templates(ctx, s.Information, services.WithTemplate(routingFS, "routing", "routing"))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Service) CreateEndpoint(ctx context.Context) error {
	defer s.Wool.Catch()
	endpoint := s.Base.BaseEndpoint(standards.REST)
	endpoint.Visibility = resources.VisibilityPublic
	rest, err := resources.LoadRestAPI(ctx, nil)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load HTTP api")
	}
	s.restEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToRestAPI(rest))
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create openapi api")
	}
	s.Endpoints = []*basev0.Endpoint{s.restEndpoint}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/routing
var routingFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
