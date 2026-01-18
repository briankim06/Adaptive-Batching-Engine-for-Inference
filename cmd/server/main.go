package main

import (
	"fmt"
	"log"
	
	"github.com/yourname/adaptive-batching-engine/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	
	fmt.Printf("Starting server on %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("Batching strategy: %s\n", cfg.Batching.Strategy)
	fmt.Printf("Worker count: %d\n", cfg.Workers.Count)
	
	// TODO: Initialize components and start server
	// See TASKS.md for implementation steps
}
