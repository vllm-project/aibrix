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
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"k8s.io/klog/v2"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/handler"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	plannerimpl "github.com/vllm-project/aibrix/apps/console/api/planner/impl"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
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
	planner    plannerapi.Planner
}

// New creates a new console Server from configuration.
func New(cfg *config.Config) *Server {
	s, err := store.NewFromURI(cfg.StoreURI, cfg.SecretsEncryptionKey)
	if err != nil {
		klog.Fatalf("Failed to construct store: %v", err)
	}
	klog.Infof("Using store %s", cfg.StoreURI)

	// Dev-mode conveniences. Seeding is opt-in so production / shared envs
	// start with an empty store.
	if cfg.DevMode {
		if seeder, ok := s.(interface{ LoadDemoData() error }); ok {
			if err := seeder.LoadDemoData(); err != nil {
				klog.Fatalf("Failed to seed demo data: %v", err)
			}
			klog.Info("Dev mode: demo data seeded")
		}
	}

	authCfg := middleware.AuthConfig{
		Mode:                      cfg.AuthMode,
		OIDCIssuerURL:             cfg.OIDCIssuerURL,
		OIDCClientID:              cfg.OIDCClientID,
		OIDCClientSecret:          cfg.OIDCClientSecret,
		OIDCRedirectURL:           cfg.OIDCRedirectURL,
		OIDCPostLogoutRedirectURL: cfg.OIDCPostLogoutRedirectURL,
		OIDCEndSessionURL:         cfg.OIDCEndSessionURL,
		OIDCGroupsClaim:           cfg.OIDCGroupsClaim,
		OIDCAdminGroups:           cfg.OIDCAdminGroups,
		OIDCAdminEmails:           cfg.OIDCAdminEmails,
		OIDCSigningAlg:            cfg.OIDCSigningAlg,
		SessionSecret:             cfg.SessionSecret,
		DevUserName:               cfg.DevUserName,
		DevUserEmail:              cfg.DevUserEmail,
		BasicUsername:             cfg.BasicUsername,
		BasicPassword:             cfg.BasicPassword,
	}

	auth, err := middleware.NewAuthMiddleware(authCfg)
	if err != nil {
		klog.Fatalf("Failed to construct auth middleware: %v", err)
	}

	return &Server{
		store: s,
		cfg:   cfg,
		auth:  auth,
	}
}

// StartGRPC starts the gRPC server on the given address.
func (s *Server) StartGRPC(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.grpcServer = grpc.NewServer()

	batchClient := plannerclient.NewOpenAIBatchClient(s.cfg.MetadataServiceURL)
	rm, err := resource_manager.NewResourceManager(rmtypes.ResourceProvisionTypeKubernetes, s.store)
	if err != nil {
		return fmt.Errorf("resource manager init: %w", err)
	}
	s.planner = plannerimpl.NewPlanner(batchClient, rm.Provisioner, plannerimpl.DefaultWorkerCount)

	// Register all service handlers
	pb.RegisterDeploymentServiceServer(s.grpcServer, handler.NewDeploymentHandler(s.store))
	pb.RegisterJobServiceServer(s.grpcServer, handler.NewJobHandler(s.store, s.planner, s.cfg.DefaultBatchModelDeploymentTemplate, s.cfg.DevMode))
	pb.RegisterModelServiceServer(s.grpcServer, handler.NewModelHandler(s.store))
	pb.RegisterModelDeploymentTemplateServiceServer(s.grpcServer, handler.NewModelDeploymentTemplateHandler(s.store))
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
		pb.RegisterModelDeploymentTemplateServiceHandlerFromEndpoint,
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
	httpHandler = corsMiddleware(s.cfg.AllowedOrigins)(httpHandler)

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
		if err := s.httpServer.Shutdown(ctx); err != nil {
			klog.Errorf("failed to shutdown http server: %v", err)
		}
	}
	if s.planner != nil {
		if err := s.planner.Close(); err != nil {
			klog.Errorf("failed to close planner: %v", err)
		}
	}
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			klog.Errorf("failed to close store: %v", err)
		}
	}
}

// corsMiddleware returns middleware that sets CORS headers from an allowlist.
//
// allowedOrigins is a comma-separated list of origins. When empty, no CORS
// headers are set (same-origin only). When the request's Origin matches an
// entry, the middleware echoes that exact origin back with credentials allowed.
// A single "*" entry permits any origin but disables credentials, since the
// "*" + Allow-Credentials combination is rejected by browsers.
func corsMiddleware(allowedOrigins string) func(http.Handler) http.Handler {
	var origins []string
	for _, o := range strings.Split(allowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	wildcard := len(origins) == 1 && origins[0] == "*"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && len(origins) > 0 {
				allowed := wildcard
				if !allowed {
					for _, o := range origins {
						if o == origin {
							allowed = true
							break
						}
					}
				}
				if allowed {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-User-ID")
					if !wildcard {
						w.Header().Set("Access-Control-Allow-Credentials", "true")
					}
				}
			}
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// staticFileMiddleware serves static files from dir for non-API paths.
// API paths (/api/) are passed through to the next handler. Non-API paths
// that don't match a real file fall back to index.html so the React Router
// SPA can render deep links (e.g. /models/abc, /batch/job-xyz) on hard
// reload or external link.
func staticFileMiddleware(dir string, next http.Handler) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	indexPath := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API requests go to the backend
		if strings.HasPrefix(r.URL.Path, "/api") {
			next.ServeHTTP(w, r)
			return
		}
		// SPA fallback: any path that doesn't map to a real file gets index.html.
		// "/" itself falls through to FileServer which serves index.html.
		if r.URL.Path != "/" {
			rel := filepath.Join(dir, filepath.Clean(r.URL.Path))
			if _, err := os.Stat(rel); os.IsNotExist(err) {
				http.ServeFile(w, r, indexPath)
				return
			}
		}
		fs.ServeHTTP(w, r)
	})
}
