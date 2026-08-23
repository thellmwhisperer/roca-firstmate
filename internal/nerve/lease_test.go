package nerve_test

import (
	"context"
	"testing"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/nerve"
)

func TestTryAcquireSeatSingleFlightAndInherit(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	ctx := context.Background()
	lease := 2 * time.Second
	firstToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	secondToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}

	held, seat, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: firstToken,
		Destination: "machine", Now: frozen, Lease: lease,
	})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !held || seat.SeatID != nerve.WatchSeatID("northwind-harbor") {
		t.Fatalf("first holder %+v held=%v", seat, held)
	}

	stolen, _, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: secondToken,
		Destination: "machine", Now: frozen.Add(time.Second), Lease: lease,
	})
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if stolen {
		t.Fatal("live lease was stolen")
	}

	renewed, err := nerve.RenewSeat(ctx, db, seat.SeatID, firstToken, frozen.Add(time.Second), lease)
	if err != nil || !renewed {
		t.Fatalf("holder renew held=%v err=%v", renewed, err)
	}
	lost, err := nerve.RenewSeat(ctx, db, seat.SeatID, secondToken, frozen.Add(time.Second), lease)
	if err != nil {
		t.Fatalf("non-holder renew: %v", err)
	}
	if lost {
		t.Fatal("non-holder renewed the watch lease")
	}

	inherited, next, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: secondToken,
		Destination: "machine", Now: frozen.Add(3 * time.Second), Lease: lease,
	})
	if err != nil {
		t.Fatalf("inherit acquire: %v", err)
	}
	if !inherited || next.SeatID != seat.SeatID {
		t.Fatalf("expired lease was not inherited held=%v seat=%+v", inherited, next)
	}
}

func TestTryAcquireSeatDoesNotStealLaterFractionalExpiry(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	firstToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	secondToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 14, 10, 0, 2, 100, time.UTC)
	held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: firstToken,
		Destination: "machine", Now: now, Lease: 100 * time.Nanosecond,
	})
	if err != nil || !held {
		t.Fatalf("first acquire held=%v err=%v", held, err)
	}
	stolen, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: secondToken,
		Destination: "machine", Now: now.Add(50 * time.Nanosecond), Lease: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stolen {
		t.Fatal("later fractional lease expiry was treated as expired")
	}
}

func TestReleaseSeatLetsTheNextHolderTakeOverImmediately(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	ctx := context.Background()
	firstToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	secondToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	held, seat, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: firstToken,
		Destination: "machine", Now: frozen, Lease: time.Minute,
	})
	if err != nil || !held {
		t.Fatalf("first acquire held=%v err=%v", held, err)
	}
	if err := nerve.ReleaseSeat(ctx, db, seat.SeatID, firstToken, frozen.Add(time.Second)); err != nil {
		t.Fatalf("release: %v", err)
	}
	inherited, _, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: secondToken,
		Destination: "machine", Now: frozen.Add(time.Second), Lease: time.Minute,
	})
	if err != nil || !inherited {
		t.Fatalf("released lease was not inherited held=%v err=%v", inherited, err)
	}
}

func TestConcurrentTryAcquireSeatYieldsOneHolder(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	start := make(chan struct{})
	type outcome struct {
		held bool
		err  error
	}
	outcomes := make(chan outcome, 8)
	for range 8 {
		go func() {
			token, err := nerve.NewHolderToken()
			if err != nil {
				outcomes <- outcome{err: err}
				return
			}
			<-start
			held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
				HomeID: "northwind-harbor", HolderToken: token,
				Destination: "machine", Now: frozen, Lease: time.Minute,
			})
			outcomes <- outcome{held: held, err: err}
		}()
	}
	close(start)
	holders := 0
	for range 8 {
		out := <-outcomes
		if out.err != nil {
			t.Fatalf("concurrent acquire: %v", out.err)
		}
		if out.held {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("holders = %d, want 1", holders)
	}
}
