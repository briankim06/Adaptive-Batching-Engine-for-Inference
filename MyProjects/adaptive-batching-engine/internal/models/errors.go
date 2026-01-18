package models

import "errors"

var (
	ErrQueueFull         = errors.New("request queue is full")
	ErrWorkerUnavailable = errors.New("no workers available")
	ErrCircuitOpen       = errors.New("circuit breaker is open")
	ErrBatchTimeout      = errors.New("batch processing timed out")
	ErrInvalidRequest    = errors.New("invalid inference request")
	ErrShuttingDown      = errors.New("server is shutting down")
)
