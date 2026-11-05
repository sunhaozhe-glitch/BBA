package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"bba.local/taskengine/internal/artex"
	"bba.local/taskengine/internal/taskengine"
)

func main() {
	listenAddr := flag.String("listen", "127.0.0.1:18421", "loopback listen address")
	origin := flag.String("origin", "http://127.0.0.1:18421", "fixed operator origin")
	resolverPath := flag.String("resolver-file", "", "0600 trusted token resolver JSON")
	developmentOperations := flag.Bool("development-operations", false, "enable authenticated task create/control/proposal routes for an isolated fixture")
	workSlots := flag.Int("task-work-slots", 1, "first-start task slot snapshot (1..8); does not enable execution")
	workerEnabled := flag.Bool("worker", false, "run the continuous deterministic Task Engine worker")
	workerPoll := flag.Duration("worker-poll", 15*time.Second, "worker poll interval (>=100ms)")
	workerBatch := flag.Int("worker-batch", 20, "bounded tasks per worker step (1..100)")
	workerRecover := flag.Bool("worker-recover", false, "run the one-shot crash recovery pass on the first worker tick")
	workerID := flag.String("worker-id", "", "stable worker id (default worker-<pid>)")
	goalFixture := flag.Bool("offline-goal-fixture", false, "isolated deterministic goal-loop fixture; never calls QQ or a real model")
	proposalExecution := flag.Bool("proposal-execution", false, "enable explicit proposal-mode materialization and worker consumption")
	loginProviderFile := flag.String("login-provider-file", "", "protected NapCat login provider configuration")
	loginFixture := flag.Bool("login-fixture", false, "use the explicit isolated login boundary fixture")
	modelProviderFile := flag.String("model-provider-file", "", "protected planning model configuration")
	modelFixture := flag.Bool("model-fixture", false, "use the explicit isolated planning model fixture")
	artexFile := flag.String("artex-provider-file", "", "protected ARTEX provider configuration; absent means not configured")
	dashboardMode := flag.String("dashboard-account-source", "projection", "projection or fixed local6300")
	dashboardMapping := flag.String("dashboard-account-mapping", "", "0600 explicit operator/account/instance bindings")
	modelRegistryPath := flag.String("model-registry-file", "", "opt-in protected workspace model registry; private parent directory required")
	modelRegistryApproved := flag.String("model-registry-approved-private-http-base", "", "trusted exact private HTTP model endpoint approval")
	flag.Parse()
	dsn := os.Getenv("BBA_TASKENGINE_DSN")
	if dsn == "" || *resolverPath == "" {
		log.Fatal("BBA_TASKENGINE_DSN and -resolver-file are required")
	}
	expectedHost, err := validateDeploymentOrigin(*origin, *listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	auth, err := loadResolver(*resolverPath)
	if err != nil {
		log.Fatal(err)
	}
	runtimeAuth, err := loadRuntimeResolver(*resolverPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := taskengine.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("open Task Engine store: %v", err)
	}
	defer store.Close()
	if *workSlots < 1 || *workSlots > taskengine.MaxTaskWorkSlots {
		log.Fatal("task-work-slots must be 1..8")
	}
	store.WorkSlotsDefault = *workSlots
	schemaVersion, err := validateServerSchema(store)
	if err != nil {
		log.Fatal(err)
	}
	loginProvider, err := taskengine.LoadLoginProvider(*loginProviderFile, *loginFixture)
	if err != nil {
		log.Fatalf("configure login provider: %v", err)
	}
	model, err := loadConfiguredModel(*modelProviderFile, *modelFixture)
	if err != nil {
		log.Fatalf("configure model provider: %v", err)
	}
	var registry *taskengine.ModelRegistry
	if *modelRegistryPath != "" {
		registry, err = taskengine.OpenModelRegistry(*modelRegistryPath, *modelRegistryApproved, model.Provider, nil)
		if err != nil {
			log.Fatalf("configure model registry: %v", err)
		}
		defer registry.Close()
		model.Provider = registry
	}
	if err := taskengine.ValidateGoalFixtureDeployment(ctx, store, *goalFixture, *developmentOperations); err != nil {
		log.Fatal(err)
	}
	goals := taskengine.NewGoalService(store, registry, *goalFixture)
	var graphProvider taskengine.GraphSyncProvider
	var graphWorker *taskengine.GraphSyncWorker
	if *artexFile != "" {
		cfg, loadErr := artex.LoadConfigFile(*artexFile)
		if loadErr != nil {
			log.Fatalf("configure ARTEX: %v", loadErr)
		}
		client, loadErr := artex.New(cfg)
		if loadErr != nil {
			log.Fatalf("configure ARTEX: %v", loadErr)
		}
		graphProvider = client
		graphWorker, err = taskengine.NewGraphSyncWorker(store, taskengine.GraphSyncWorkerConfig{
			WorkerID: fmt.Sprintf("graph-%d", os.Getpid()), Provider: graphProvider,
			BatchSize: *workerBatch, PollInterval: *workerPoll, TickTimeout: 30 * time.Second,
			Logf: log.Printf,
		})
		if err != nil {
			log.Fatalf("configure graph sync worker: %v", err)
		}
	}

	// The development worker is fixture transport only: it advances the local
	// Task Engine chain and performs no QQ action. A real adapter is never
	// selected here and needs its own explicit write authorization.
	var worker *taskengine.Worker
	workerName := "off"
	if *workerEnabled {
		name := *workerID
		if name == "" {
			name = fmt.Sprintf("worker-%d", os.Getpid())
		}
		steps := taskengine.DefaultWorkerSteps()
		steps.Recover = *workerRecover
		steps.Proposal = *proposalExecution
		worker, err = taskengine.NewWorker(store, taskengine.WorkerConfig{
			WorkerID:          name,
			PollInterval:      *workerPoll,
			BatchSize:         *workerBatch,
			Transport:         taskengine.WorkerFixtureTransport,
			Steps:             steps,
			ProposalExecution: *proposalExecution,
			Logf:              func(format string, args ...any) { log.Printf(format, args...) },
		})
		if err != nil {
			log.Fatalf("configure Task Engine worker: %v", err)
		}
		workerName = name
	}

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		log.Fatal("listener did not bind a loopback address")
	}
	environment := "unconfigured"
	if *loginFixture || *modelFixture {
		environment = "fixture"
	}
	runtimeConfig := runtimeSurfaceConfig{Environment: environment,
		DevelopmentOperations: *developmentOperations, ProposalExecution: *proposalExecution,
		Login: loginProvider != nil, WorkerEnabled: *workerEnabled, WorkerID: workerName,
		WorkerPoll: *workerPoll, WorkerBatch: *workerBatch, Model: model}
	api := serverAPISurface(serverRoutes{
		WorkSettings:          store.WorkSettingsHandler(auth, taskengine.WorkSettingsRuntime{WorkerEnabled: *workerEnabled, ProposalEnabled: *proposalExecution, Transport: string(taskengine.WorkerFixtureTransport)}),
		OperatorConversations: store.OperatorConversationHandler(operatorConversationAuth(auth, runtimeAuth)),
		Operator:              store.Handler(auth), Proposal: store.ProposalHandler(auth, *developmentOperations),
		Orchestration: store.OrchestrationHandler(auth, *proposalExecution),
		Graph:         store.GraphSyncHandler(auth, graphProvider), Login: store.LoginHandler(auth, loginProvider),
		Model:     store.ModelHandler(auth, model.Provider, model.Provider != nil),
		Runtime:   operatorRuntimeHandler(store, auth, runtimeConfig),
		Report:    store.GroupReportHandler(auth),
		Summaries: store.GroupSummaryRuntimeHandler(runtimeAuth), Chat: store.ChatRuntimeHandler(runtimeAuth),
		Permits: store.ChatPermitRuntimeHandler(runtimeAuth), DevelopmentOperations: *developmentOperations,
	})
	dashboardSource, err := loadDashboardSource(*dashboardMode, *dashboardMapping)
	if err != nil {
		log.Fatal(err)
	}
	var productAPI http.Handler = api
	if registry != nil {
		models := registry.Handler(auth, store)
		configs := registry.ConfigHandler(auth, store)
		agentModel := registry.AgentModelHandler(auth, store)
		productAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/operator/models" || strings.HasPrefix(r.URL.Path, "/v1/operator/models/") {
				models.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == "/v1/operator/agents" || strings.HasPrefix(r.URL.Path, "/v1/operator/agents/") || r.URL.Path == "/v1/operator/skills" || strings.HasPrefix(r.URL.Path, "/v1/operator/skills/") {
				configs.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == "/v1/agent-model-requests" {
				agentModel.ServeHTTP(w, r)
				return
			}
			api.ServeHTTP(w, r)
		})
	}
	goalAPI := goals.Handler(auth)
	baseProductAPI := productAPI
	productAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/operator/goal-runs" || strings.HasPrefix(r.URL.Path, "/v1/operator/goal-runs/") {
			goalAPI.ServeHTTP(w, r)
			return
		}
		baseProductAPI.ServeHTTP(w, r)
	})
	if *goalFixture {
		go func() {
			if err := goals.Run(ctx); err != nil {
				log.Printf("offline goal loop stopped: %s", taskengine.Code(err))
			}
		}()
	}
	contentAPI := store.ContentHandler(auth)
	productAPI = systemSettingsSurface(productAPI, registry, auth, store)
	previousAPI := productAPI
	productAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/operator/content-rules") || strings.HasPrefix(r.URL.Path, "/v1/operator/content-reviews") {
			contentAPI.ServeHTTP(w, r)
			return
		}
		previousAPI.ServeHTTP(w, r)
	})
	handler := consoleSurface(productSurface(productAPI, store.DashboardHandler(auth, dashboardSource), auth))
	server := &http.Server{
		Handler:           fixedHost(handler, expectedHost),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	workerDone := make(chan error, 1)
	graphDone := make(chan error, 1)
	if graphWorker != nil {
		go func() { graphDone <- graphWorker.Run(ctx) }()
	}
	if worker != nil {
		go func() { workerDone <- worker.Run(ctx) }()
	}
	fmt.Printf("taskengine listening origin=%s schema=%d pid=%d worker=%s\n", *origin, schemaVersion, os.Getpid(), workerName)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	if worker != nil {
		// The worker stops on the same signal context and drains only already
		// produced notifications; wait for it before the store is closed.
		if err := <-workerDone; err != nil {
			log.Printf("Task Engine worker stopped with error: %v", err)
		}
	}
	if graphWorker != nil {
		if err := <-graphDone; err != nil {
			log.Printf("Graph sync worker stopped with error: %v", err)
		}
	}
}

// Startup validates the full product schema before providers, listeners or workers.
// B4 settings and first-start consumption require schema 24, even though the
// server retains minimum-version compatibility with future additive migrations.
// Operators must run taskengine-migrate explicitly; startup never migrates.
const serverMinimumSchema = 24

// validateServerSchema refuses a database that never ran the required
// migrations. A version is only observed after its migration committed, and
// startup never migrates implicitly.
func validateServerSchema(store *taskengine.Store) (int, error) {
	var version int
	if err := store.Pool.QueryRow(context.Background(), `SELECT max(version) FROM core.schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read Task Engine schema version: %w", err)
	}
	if version < serverMinimumSchema {
		return version, fmt.Errorf("Task Engine requires schema >= %d (apply taskengine-migrate first); found %d", serverMinimumSchema, version)
	}
	return version, nil
}
