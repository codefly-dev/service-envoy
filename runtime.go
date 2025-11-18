package main

import (
	"context"
	"github.com/codefly-dev/core/agents/helpers/code"
	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
)

type Runtime struct {
	*Service

	// internal
	runner *runners.DockerEnvironment
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if req.DisableCatch {
		s.Wool.DisableCatch()
	}

	s.Runtime.SetEnvironment(req.Environment)

	err = s.Setup(ctx)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.Wool.Debug("loading service")

	err = s.LoadRestRoutes(ctx)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	err = s.LoadGRPCRoutes(ctx)
	if err != nil {
		// gRPC routes are optional, so we don't fail if the directory doesn't exist
		s.Wool.Debug("no gRPC routes found or error loading", wool.ErrField(err))
	}

	err = s.LoadCustomRoutes(ctx)
	if err != nil {
		// Custom routes are optional
		s.Wool.Debug("no custom routes found or error loading", wool.ErrField(err))
	}

	s.Endpoints, err = s.Base.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.restEndpoint, err = resources.FindRestEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "finding REST endpoint")
	}
	if s.restEndpoint == nil {
		return s.Runtime.LoadError(s.Wool.NewError("cannot find REST endpoint"))
	}

	//if s.Settings.Watch && s.Watcher == nil {
	//	s.Wool.Debug("setting up code watcher")
	//	// Add proto and swagger
	//	dependencies := requirements.Clone()
	//	dependencies.Localize(s.Location)
	//	conf := services.NewWatchConfiguration(dependencies)
	//	err = s.SetupWatcher(ctx, conf, s.EventHandler)
	//	if err != nil {
	//		s.Wool.Warn("error in watcher", wool.ErrField(err))
	//	}
	//}

	return s.Runtime.LoadResponse()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	s.Wool.Debug("REST endpoint", wool.Field("summary", resources.MakeEndpointSummary(s.restEndpoint)))

	if s.requiresAuth {
		confs := append([]*v0.Configuration{req.Configuration}, req.WorkspaceConfigurations...)
		s.Wool.Focus("creating validators from configurations", wool.SliceCountField(confs))
		validators, err := s.CreateValidators(ctx, confs...)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot create validators")
		}
		s.validators = validators

	}
	
	// Store dependency endpoints for use in Start
	s.Base.DependencyEndpoints = req.DependenciesEndpoints

	s.Wool.Debug("looking for network instance", wool.Field("endpoint", resources.MakeEndpointSummary(s.restEndpoint)))

	s.NetworkMappings = req.ProposedNetworkMappings

	net, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.restEndpoint, resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Runtime.InitErrorf(err, "cannot find network instance: %v", resources.MakeManyNetworkMappingSummary(s.NetworkMappings))
	}

	s.Infof("will run on: %s", net.Address)

	// for docker
	s.port = 80

	if s.runner != nil {
		err = s.runner.Stop(ctx)
		if err != nil {
			return s.Runtime.InitError(err)
		}
	}

	runner, err := runners.NewDockerHeadlessEnvironment(ctx, runtimeImage, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.runner = runner

	s.runner.WithMount(s.Local("routing"), "/codefly/routing")
	s.runner.WithPortMapping(ctx, uint16(net.Port), s.port)
	// Expose admin port for debugging
	s.runner.WithPortMapping(ctx, 9901, 9901)

	// Envoy doesn't need special environment variables like KrakenD
	// Configuration is passed via -c flag

	s.runner.WithCommand("envoy", "-c", "/codefly/routing/config/envoy.yaml")

	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogStartRequest(req)

	// Envoy always runs in Docker, so use ContainerNetworkAccess to reach backends
	// This allows Envoy (in Docker) to reach backends on the host via host.docker.internal
	err := s.writeConfigWithEndpoints(ctx, req.DependenciesNetworkMappings, s.Base.DependencyEndpoints, resources.NewContainerNetworkAccess())
	if err != nil {
		return s.Runtime.StartError(err)
	}

	// Use the passed context instead of Background for proper cancellation
	runContext := ctx

	err = s.runner.Init(runContext)

	if err != nil {
		return s.Runtime.StartError(err)
	}

	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("stopping service")
	if s.runner != nil {
		err := s.runner.Stop(ctx)
		if err != nil {
			return s.Runtime.StopError(err)
		}

		err = s.runner.Shutdown(ctx)
		if err != nil {
			return s.Runtime.StopError(err)
		}
	}

	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	s.Wool.Debug("event detected", wool.Field("event", event))
	s.Runtime.DesiredLoad()
	return nil
}
