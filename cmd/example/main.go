package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"go-recovery/dletter"
)

type ReservationDeadletter struct {
	Type string `json:"type"`
	SpID string `json:"sp_id"`
	Qty  int    `json:"qty"`
}

func (r ReservationDeadletter) AppendLog(buf []byte) []byte {
	buf = append(buf, `{"type":"`...)
	buf = append(buf, r.Type...)
	buf = append(buf, `","sp_id":"`...)
	buf = append(buf, r.SpID...)
	buf = append(buf, `","qty":`...)
	buf = strconv.AppendInt(buf, int64(r.Qty), 10)
	buf = append(buf, `}`...)
	return buf
}

func main() {
	fmt.Println("=== Starting R-DLQ System ===")

	dlq, err := dletter.New("logs/dead_reservations.log",
		dletter.WithMaxSize(10),
		dletter.WithCompress(true),
	)
	if err != nil {
		log.Fatalf("Failed to init DLQ: %v", err)
	}
	defer dlq.Close()

	fmt.Println("[Main App] Attempting to reserve inventory...")
	failedRes := ReservationDeadletter{Type: "reserve", SpID: "PROD-99", Qty: 5}
	dbErr := errors.New("database timeout: connection refused")

	fmt.Println("[Main App] Database failed! Routing to DLQ...")
	err = dletter.Log(dlq, failedRes, dbErr, 1)
	if err != nil {
		log.Fatalf("Critical: Failed to write to DLQ: %v", err)
	}

	time.Sleep(1 * time.Second)
	fmt.Println("\n=== Waking up Background Recovery Worker ===")

	recoveryHandler := func(payload []byte) error {
		var res ReservationDeadletter

		if err := json.Unmarshal(payload, &res); err != nil {
			return fmt.Errorf("corrupted payload: %w", err)
		}

		fmt.Printf("[Worker] Successfully parsed payload! Type: %s, SpID: %s, Qty: %d\n", res.Type, res.SpID, res.Qty)
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
