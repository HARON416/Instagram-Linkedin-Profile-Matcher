package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVisitDelayRangeAndCancellation(t *testing.T) {
	for range 100 {
		if delay := randomVisitDelay(2*time.Second, 5*time.Second); delay < 2*time.Second || delay > 5*time.Second {
			t.Fatalf("delay outside configured range: %s", delay)
		}
	}
	if randomVisitDelay(0, 0) != 0 || randomVisitDelay(time.Second, time.Second) != time.Second {
		t.Fatal("fixed delay or disabled delay is incorrect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitVisitDelay(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want immediate cancellation", err)
	}
	if err := waitVisitDelay(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidVisitDelaysAreRejected(t *testing.T) {
	for _, durations := range [][2]time.Duration{{-1, time.Second}, {time.Second, 0}} {
		options := runOptions{DataDir: "data", BrowserProfiles: 1, Timeout: time.Second, DelayMin: durations[0], DelayMax: durations[1]}
		if options.validate() == nil {
			t.Fatalf("invalid delay range accepted: %v", durations)
		}
	}
}
