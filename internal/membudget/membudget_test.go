package membudget

import (
	"errors"
	"testing"
	"time"
)

func TestAcquireTimesOut(t *testing.T) {
	b := newBudget(100)

	rel, err := b.acquire(80, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()

	if _, err := b.acquire(50, 50*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}

	if time.Since(start) > time.Second {
		t.Fatal("waited too long")
	}

	rel()

	if _, err := b.acquire(50, 50*time.Millisecond); err != nil {
		t.Fatalf("after release: %v", err)
	}

	if _, err := b.acquire(101, time.Millisecond); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestAcquireWakesOnRelease(t *testing.T) {
	b := newBudget(100)
	rel, _ := b.acquire(100, time.Second)

	go func() { time.Sleep(20 * time.Millisecond); rel() }()

	if _, err := b.acquire(10, time.Second); err != nil {
		t.Fatal(err)
	}
}
