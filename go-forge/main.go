package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

func main() {
	// Reduce GC frequency inside the 1024 MiB sandbox for minimal p50 latency jitter
	debug.SetGCPercent(800)
	debug.SetMemoryLimit(768 << 20)
	prewarmPools()

	listenAddr := os.Getenv("FORGE_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:0"
	}

	store := NewForgeStore()
	srv := NewServer(store)
	port, err := srv.Listen(listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[go-forge] failed to listen: %v\n", err)
		os.Exit(1)
	}

	// Signal bound port to launcher (main.ts)
	fmt.Printf("READY %d\n", port)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		srv.Close()
		os.Exit(0)
	}()

	srv.Serve()
}
