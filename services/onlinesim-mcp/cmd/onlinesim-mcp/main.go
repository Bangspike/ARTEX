package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Autumn-27/artex/services/onlinesim-mcp/internal/broker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "mock", "mock or live (mock never makes network requests)")
	transport := flag.String("transport", "stdio", "stdio or http")
	listen := flag.String("listen", "127.0.0.1:8790", "HTTP bind address")
	dataDir := flag.String("data-dir", "", "private state directory (default ./data/<mode>)")
	taskID := flag.String("task-id", os.Getenv("ARTEX_TASK_ID"), "trusted task identity for stdio")
	allowLive := flag.Bool("allow-live-orders", false, "explicitly allow paid orders in live mode")
	recoverID := flag.String("recover-order", "", "operator-only ambiguous broker order to recover")
	recoverTZID := flag.String("recover-tzid", "", "verified OnlineSim operation ID")
	maxOrders := flag.Int("max-orders-per-task", 2, "maximum purchase attempts per task, across restarts")
	maxPrice := flag.String("max-price", "1.00", "maximum preflight price in profile tariff units")
	budget := flag.String("task-budget", "5.00", "gross preflight tariff budget per task; no assumed refunds")
	unit := flag.String("price-unit", "profile_tariff_units", "label matching the OnlineSim profile tariff currency")
	poll := flag.Duration("poll-interval", 5*time.Second, "background polling interval")
	lease := flag.Duration("wait-timeout", 10*time.Minute, "default and maximum wait deadline (up to 15m)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *mode != "mock" && *mode != "live" {
		return errors.New("mode must be mock or live")
	}
	if *transport != "stdio" && *transport != "http" {
		return errors.New("transport must be stdio or http")
	}
	if *taskID == "" && *mode == "mock" {
		*taskID = "demo-task"
	}
	if (*transport == "stdio" || *recoverID != "") && !broker.ValidateOwner(*taskID) {
		return errors.New("valid task-id or ARTEX_TASK_ID required")
	}
	if *transport == "http" && len(os.Getenv("ONLINESIM_MCP_TOKEN")) < 16 {
		return errors.New("ONLINESIM_MCP_TOKEN of at least 16 characters required for HTTP")
	}
	var provider broker.Provider
	if *mode == "mock" {
		provider = broker.NewMockProvider()
	} else {
		if !*allowLive {
			return errors.New("live mode requires allow-live-orders; API Key configuration alone does not enable purchases")
		}
		var err error
		provider, err = broker.NewHTTPProvider("https://onlinesim.io", os.Getenv("ONLINESIM_API_KEY"), nil)
		if err != nil {
			return err
		}
	}
	cfg := broker.DefaultConfig()
	var err error
	cfg.MaxOrders = *maxOrders
	cfg.MaxPrice, err = broker.ParseAmount(*maxPrice)
	if err != nil {
		return err
	}
	cfg.TaskBudget, err = broker.ParseAmount(*budget)
	if err != nil {
		return err
	}
	cfg.Currency = *unit
	cfg.PollInterval = *poll
	cfg.Lease = *lease
	if *mode == "live" && *poll < 5*time.Second {
		return errors.New("live poll-interval must be at least 5s")
	}
	if *dataDir == "" {
		*dataDir = filepath.Join("data", *mode)
	}
	store, err := broker.OpenStore(*dataDir, *mode)
	if err != nil {
		return err
	}
	defer store.Close()
	b, err := broker.New(provider, store, cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *recoverID != "" || *recoverTZID != "" {
		if *recoverID == "" || *recoverTZID == "" {
			return errors.New("both recovery flags are required")
		}
		v, err := b.Recover(ctx, *taskID, *recoverID, *recoverTZID)
		if err != nil {
			return err
		}
		fmt.Printf("recovered order %s; status=%s\n", v.OrderID, v.Status)
		return nil
	}
	pollCtx, cancelPoll := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); b.Run(pollCtx) }()
	defer func() { cancelPoll(); wg.Wait() }()
	if *transport == "stdio" {
		err := broker.NewMCP(b, *taskID).Run(ctx, &mcp.StdioTransport{})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	if strings.ContainsAny(os.Getenv("ONLINESIM_MCP_TOKEN"), "\r\n") {
		return errors.New("invalid MCP token")
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", broker.HTTPHandler(b, os.Getenv("ONLINESIM_MCP_TOKEN")))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !b.Healthy() {
			http.Error(w, "state unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","mode":%q}`, *mode)
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Printf("OnlineSim MCP mode=%s listening at %s", *mode, *listen)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
