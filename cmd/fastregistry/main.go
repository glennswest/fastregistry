package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gwest/fastregistry/config"
	"github.com/gwest/fastregistry/internal/api"
	"github.com/gwest/fastregistry/internal/api/ui"
	"github.com/gwest/fastregistry/internal/certs"
	"github.com/gwest/fastregistry/internal/events"
	"github.com/gwest/fastregistry/internal/mirror"
	"github.com/gwest/fastregistry/internal/releases"
	"github.com/gwest/fastregistry/internal/storage"
	"github.com/gwest/fastregistry/internal/sync"
)

var (
	version   = "0.6.0"
	buildTime = "unknown"
)

func main() {
	// Parse flags
	configFile := flag.String("config", "", "Path to config file")
	showVersion := flag.Bool("version", false, "Show version")
	storagePath := flag.String("storage", "", "Storage path (overrides config)")
	listenAddr := flag.String("addr", "", "Listen address (overrides config)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("FastRegistry %s (built %s)\n", version, buildTime)
		os.Exit(0)
	}

	// Load configuration
	var cfg *config.Config
	var err error

	if *configFile != "" {
		cfg, err = config.Load(*configFile)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		cfg = config.DefaultConfig()
	}

	// Override with flags
	if *storagePath != "" {
		cfg.Storage.Path = *storagePath
	}
	if *listenAddr != "" {
		cfg.Server.Addr = *listenAddr
	}

	// Initialize storage
	log.Printf("Initializing storage at %s", cfg.Storage.Path)

	blobs, err := storage.NewBlobStore(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("Failed to initialize blob store: %v", err)
	}
	defer blobs.Close()

	metadata, err := storage.NewMetadataStore(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("Failed to initialize metadata store: %v", err)
	}
	defer metadata.Close()

	// Initialize event store
	eventStore := events.NewStore(metadata)

	uploads, err := storage.NewUploadManager(cfg.Storage.Path)
	if err != nil {
		log.Fatalf("Failed to initialize upload manager: %v", err)
	}

	// Initialize authentication
	var auth api.Authenticator
	switch cfg.Auth.Type {
	case "htpasswd":
		auth, err = api.NewHtpasswdAuth(cfg.Auth.HtpasswdFile)
		if err != nil {
			log.Fatalf("Failed to initialize auth: %v", err)
		}
		log.Printf("Authentication: htpasswd (%s)", cfg.Auth.HtpasswdFile)
	default:
		auth = &api.NoAuth{}
		log.Printf("Authentication: disabled")
	}

	// Initialize mirror proxy
	var proxy *mirror.Proxy
	if len(cfg.Mirrors) > 0 {
		proxy = mirror.NewProxy(cfg.Mirrors, blobs, metadata)
		log.Printf("Mirror proxy enabled for %d upstreams", len(cfg.Mirrors))
	}

	// Initialize sync scheduler
	var scheduler *sync.Scheduler
	if len(cfg.Sync.Sources) > 0 {
		scheduler = sync.NewScheduler(cfg.Sync.Sources, blobs, metadata)
		scheduler.Start()
		log.Printf("Sync scheduler started with %d sources", len(cfg.Sync.Sources))
		defer scheduler.Stop()
	}

	// Initialize certificate manager
	var certMgr *certs.Manager
	certMgr, err = certs.NewManager(cfg.Certs)
	if err != nil {
		log.Printf("Warning: certificate manager init failed: %v", err)
	}

	// Initialize release manager
	var releaseMgr *releases.Manager
	if cfg.Releases.Enabled {
		releaseMgr = releases.NewManager(cfg.Releases, blobs, metadata)
		releaseMgr.SetEventStore(eventStore)
		releaseMgr.Start()
		log.Printf("Release manager started (upstream: %s/%s)", cfg.Releases.Upstream, cfg.Releases.Repository)
		defer releaseMgr.Stop()
	}

	// Create UI handler
	uiHandler := ui.NewHandler(metadata, blobs, scheduler, releaseMgr, certMgr, eventStore)

	// Create replication exporter
	exporter := sync.NewExporter(releaseMgr, eventStore, version)

	// Create router
	router := api.NewRouter(blobs, metadata, uploads, auth)
	router.SetProxy(proxy)
	router.SetScheduler(scheduler)
	router.SetUI(uiHandler)
	router.SetReleaseManager(releaseMgr)
	router.SetCertManager(certMgr)
	router.SetExporter(exporter)

	// Set up file server for release artifacts and generated ISOs
	if cfg.Releases.Enabled {
		releasesHandler := http.StripPrefix("/files/releases/", http.FileServer(http.Dir(cfg.Releases.ArtifactPath)))
		installsHandler := http.StripPrefix("/files/installs/", http.FileServer(http.Dir(releaseMgr.InstallsPath())))

		// Combined handler for /files/*
		filesHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/files/installs/") {
				installsHandler.ServeHTTP(w, r)
			} else {
				releasesHandler.ServeHTTP(w, r)
			}
		})

		trackingHandler := api.NewTrackingFileServer(filesHandler, eventStore)
		router.SetFilesHandler(trackingHandler)
	}

	// Create HTTP server
	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // No timeout for blob uploads
		WriteTimeout:      0, // No timeout for blob downloads
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	// Configure TLS if certificates are provided
	if cfg.Server.TLS.Cert != "" && cfg.Server.TLS.Key != "" {
		server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			},
		}
	}

	// Handle graceful shutdown
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	// Start server
	go func() {
		log.Printf("FastRegistry %s starting on %s", version, cfg.Server.Addr)

		var err error
		if cfg.Server.TLS.Cert != "" {
			log.Printf("TLS enabled")
			err = server.ListenAndServeTLS(cfg.Server.TLS.Cert, cfg.Server.TLS.Key)
		} else {
			log.Printf("TLS disabled (use -config to enable)")
			err = server.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Start additional server if configured (e.g., port 80)
	var server2 *http.Server
	if cfg.Server.AlsoListen != "" {
		server2 = &http.Server{
			Addr:              cfg.Server.AlsoListen,
			Handler:           router,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       0,
			WriteTimeout:      0,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
		go func() {
			log.Printf("Also listening on %s", cfg.Server.AlsoListen)
			if err := server2.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Secondary server error: %v", err)
			}
		}()
	}

	// Wait for shutdown signal
	<-shutdown
	log.Printf("Shutting down...")

	// Give connections 30 seconds to finish
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}

	if server2 != nil {
		if err := server2.Shutdown(ctx); err != nil {
			log.Printf("Secondary server shutdown error: %v", err)
		}
	}

	log.Printf("Shutdown complete")
}
