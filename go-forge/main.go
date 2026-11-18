package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

func main() {
	// Match container CPU quota and minimize GC pauses during high-throughput bursts
	tuneRuntimeForContainer()
	debug.SetGCPercent(1000)
	debug.SetMemoryLimit(768 << 20)
	startTimeCache()
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
