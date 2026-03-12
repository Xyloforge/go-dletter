package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Xyloforge/go-dletter/dletter"
)

// Order is a plain struct with json tags — no Loggable interface needed.
// dletter.Log serializes it automatically via json.Marshal.
type Order struct {
	ID  string `json:"id"`
	Qty int    `json:"qty"`
}

func main() {
	fmt.Println("=== Starting DLQ System ===")

	dlq, err := dletter.New("logs/orders.log",
		dletter.WithMaxSize(10),
		dletter.WithCompress(true),
	)
	if err != nil {
		log.Fatalf("Failed to init DLQ: %v", err)
	}
	defer dlq.Close()

	fmt.Println("[Main App] Attempting to save order...")
	order := Order{ID: "ord-42", Qty: 5}
	dbErr := errors.New("database timeout: connection refused")

	fmt.Println("[Main App] Database failed! Routing to DLQ...")
	if err := dletter.Log(dlq, order, dbErr, 1); err != nil {
		log.Fatalf("Critical: Failed to write to DLQ: %v", err)
	}

	time.Sleep(1 * time.Second)
	fmt.Println("\n=== Waking up Background Recovery Worker ===")

	recoveryHandler := func(payload []byte) error {
		var o Order
		if err := json.Unmarshal(payload, &o); err != nil {
			return fmt.Errorf("corrupted payload: %w", err)
		}

		fmt.Printf("[Worker] Recovered order: ID=%s, Qty=%d\n", o.ID, o.Qty)
		fmt.Println("[Worker] Re-attempting database save... SUCCESS!")
		return nil
	}

	opts := dletter.ReplayOptions{
		InitialWait: 500 * time.Millisecond,
		MaxAttempts: 3,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := dlq.Replay(ctx, recoveryHandler, opts); err != nil {
		log.Printf("[Worker] Replay stopped: %v", err)
	}

	fmt.Println("=== System Shutdown ===")
}
