package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	assets "acestreamhub"
	"acestreamhub/internal/channels"
	"acestreamhub/internal/config"
	"acestreamhub/internal/db"
	"acestreamhub/internal/scanner"
	"acestreamhub/internal/scheduler"
	"acestreamhub/internal/stream"
	"acestreamhub/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	webPort := os.Getenv("WEB_PORT")
	if webPort == "" {
		webPort = "5004"
	}

	cfgMgr := config.NewManager(dataDir)
	cfgMgr.SetupSecurity()

	dbStore, err := db.Open(dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize SQLite database: %v", err)
	}
	defer dbStore.Close()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chanStore := channels.NewStore(cfgMgr, dbStore)
	hub := stream.NewHub(rootCtx, cfgMgr, dbStore, chanStore)
	scanSvc := scanner.NewScanner(cfgMgr, dbStore, chanStore, hub)
	sched := scheduler.NewScheduler(rootCtx, cfgMgr, chanStore)
	sched.Init()

	server, err := web.NewServer(assets.FS, cfgMgr, dbStore, chanStore, hub, scanSvc, sched)
	if err != nil {
		log.Fatalf("Failed to initialize web server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              ":" + webPort,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down cleanly...", sig)
		cancel()
		hub.CleanupAll()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = httpServer.Shutdown(shutCtx)
	}()

	log.Printf("AceStream Hub (Go) listening on :%s", webPort)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server failed: %v", err)
	}
}
