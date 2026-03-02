/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"net"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"k8s.io/klog/v2"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/handler"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"github.com/vllm-project/aibrix/apps/console/api/store"

	"github.com/vllm-project/aibrix/apps/console/api/config"
)

// Server holds the gRPC and HTTP servers for the console backend.
type Server struct {
	grpcServer *grpc.Server
	httpServer *http.Server
	store      store.Store
	cfg        *config.Config
	auth       *middleware.AuthMiddleware
	mysqlStore *store.MySQLStore // nil if using memory store
}

// New creates a new console Server from configuration.
func New(cfg *config.Config) *Server {
	var s store.Store
	var mysqlStore *store.MySQLStore

	switch cfg.StoreType {
	case "mysql":
		ms, err := store.NewMySQLStore(cfg.MySQLDSN)
		if err != nil {
			klog.Fatalf("Failed to connect to MySQL: %v", err)
		}
		if err := ms.RunMigrations("apps/console/api/store/migrations"); err != nil {
			klog.Fatalf("Failed to run MySQL migrations: %v", err)
		}
		if err := ms.SeedData(); err != nil {
			klog.Fatalf("Failed to seed MySQL data: %v", err)
		}
		s = ms
		mysqlStore = ms
		klog.Info("Using MySQL store")
	default:
		s = store.NewMemoryStore()
		klog.Info("Using in-memory store")
	}

	authCfg := middleware.AuthConfig{
		Mode:             cfg.AuthMode,
		OIDCIssuerURL:    cfg.OIDCIssuerURL,
		OIDCClientID:     cfg.OIDCClientID,
		OIDCClientSecret: cfg.OIDCClientSecret,
		OIDCRedirectURL:  cfg.OIDCRedirectURL,
		SessionSecret:    cfg.SessionSecret,
		DevUserName:      cfg.DevUserName,
		DevUserEmail:     cfg.DevUserEmail,
		BasicUsername:    cfg.BasicUsername,
		BasicPassword:    cfg.BasicPassword,
	}

	return &Server{
		store:      s,
		cfg:        cfg,
		auth:       middleware.NewAuthMiddleware(authCfg),
		mysqlStore: mysqlStore,
	}
}

// StartGRPC starts the gRPC server on the given address.
func (s *Server) StartGRPC(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.grpcServer = grpc.NewServer()

	// Register all service handlers
	pb.RegisterDeploymentServiceServer(s.grpcServer, handler.NewDeploymentHandler(s.store))
	pb.RegisterJobServiceServer(s.grpcServer, handler.NewJobHandler(s.store))
	pb.RegisterModelServiceServer(s.grpcServer, handler.NewModelHandler(s.store))
	pb.RegisterAPIKeyServiceServer(s.grpcServer, handler.NewAPIKeyHandler(s.store))
	pb.RegisterSecretServiceServer(s.grpcServer, handler.NewSecretHandler(s.store))
	pb.RegisterQuotaServiceServer(s.grpcServer, handler.NewQuotaHandler(s.store))

	// Enable gRPC reflection for debugging
	reflection.Register(s.grpcServer)

	klog.Infof("gRPC server listening on %s", addr)
	return s.grpcServer.Serve(lis)
}

// StartHTTP starts the grpc-gateway HTTP server on the given address,
// connecting to the gRPC server at grpcAddr.
func (s *Server) StartHTTP(httpAddr, grpcAddr string) error {
	ctx := context.Background()

	mux := runtime.NewServeMux()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	// Register all gRPC-gateway handlers
	for _, registerFn := range []func(context.Context, *runtime.ServeMux, string, []grpc.DialOption) error{
		pb.RegisterDeploymentServiceHandlerFromEndpoint,
		pb.RegisterJobServiceHandlerFromEndpoint,
		pb.RegisterModelServiceHandlerFromEndpoint,
		pb.RegisterAPIKeyServiceHandlerFromEndpoint,
		pb.RegisterSecretServiceHandlerFromEndpoint,
		pb.RegisterQuotaServiceHandlerFromEndpoint,
	} {
		if err := registerFn(ctx, mux, grpcAddr, opts); err != nil {
			return err
		}
	}

	// Register playground SSE proxy as a custom route
	playgroundHandler := handler.NewPlaygroundHandler(s.cfg.GatewayEndpoint)
	if err := mux.HandlePath("POST", "/api/v1/playground/chat/completions", playgroundHandler.HandleChatCompletion); err != nil {
		return err
	}

	// Register file proxy routes
	fileHandler := handler.NewFileHandler(s.cfg.MetadataServiceURL)
	fileHandler.RegisterRoutes(mux)

	// Register auth routes
	s.auth.RegisterAuthRoutes(mux)

	// Register health endpoint
	if err := mux.HandlePath("GET", "/api/v1/health", func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}); err != nil {
		return err
	}

	// Build handler chain: CORS -> Auth -> Routes
	var httpHandler http.Handler = mux
	httpHandler = s.auth.Handler(httpHandler)
	httpHandler = corsMiddleware(httpHandler)

	// Serve static files if configured
	if s.cfg.StaticFilesDir != "" {
		httpHandler = staticFileMiddleware(s.cfg.StaticFilesDir, httpHandler)
	}

	s.httpServer = &http.Server{
		Addr:    httpAddr,
		Handler: httpHandler,
	}

	klog.Infof("HTTP gateway listening on %s", httpAddr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops both servers.
func (s *Server) Shutdown(ctx context.Context) {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.httpServer != nil {
		_ = s.httpServer.Shutdown(ctx)
	}
	if s.mysqlStore != nil {
		_ = s.mysqlStore.Close()
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-User-ID")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// staticFileMiddleware serves static files from dir for non-API paths.
// API paths (/api/) are passed through to the next handler.
func staticFileMiddleware(dir string, next http.Handler) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API requests go to the backend
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			next.ServeHTTP(w, r)
			return
		}
		// Try to serve static file
		fs.ServeHTTP(w, r)
	})
}
