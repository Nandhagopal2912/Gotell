package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"gotel/worker"
)

func result(id int) worker.JobResult {
	return worker.JobResult{Job: worker.Job{TargetID: id}}
}

func TestSendReceiveInOrder(t *testing.T) {
	c := New(3)
	for i := 1; i <= 3; i++ {
		if err := c.Send(context.Background(), result(i)); err != nil {
			t.Fatalf("Send(%d): %v", i, err)
		}
	}
	c.Close()

	want := 1
	for jr := range c.Records() {
		if jr.Job.TargetID != want {
			t.Errorf("got target %d, want %d", jr.Job.TargetID, want)
		}
		want++
	}
	if st := c.Stats(); st.Sent != 3 || st.Dropped != 0 || st.Depth != 0 {
		t.Errorf("Stats = %+v, want Sent 3, Dropped 0, Depth 0", st)
	}
}

func TestStatsShowDepthAndCapacity(t *testing.T) {
	c := New(5)
	for i := 0; i < 3; i++ {
		c.Send(context.Background(), result(i))
	}
	if st := c.Stats(); st.Depth != 3 || st.Capacity != 5 {
		t.Errorf("Depth = %d, Capacity = %d, want 3 and 5", st.Depth, st.Capacity)
	}
}

// Phase 5 exit criterion: saturation does not silently drop records.
func TestBackpressureWaitsInsteadOfDropping(t *testing.T) {
	const total = 10
	c := New(2) // much smaller than the number of records

	// A slow reader, like a busy database writer.
	received := make(chan int)
	go func() {
		n := 0
		for range c.Records() {
			n++
			time.Sleep(20 * time.Millisecond)
		}
		received <- n
	}()

	for i := 0; i < total; i++ {
		if err := c.Send(context.Background(), result(i)); err != nil {
			t.Fatalf("Send(%d): %v", i, err)
		}
	}
	c.Close()

	if n := <-received; n != total {
		t.Errorf("reader got %d records, want %d", n, total)
	}
	st := c.Stats()
	if st.Dropped != 0 || st.Sent != total {
		t.Errorf("Stats = %+v, want Sent %d and Dropped 0", st, total)
	}
	if st.MaxWait < 10*time.Millisecond {
		t.Errorf("MaxWait = %v, want the sender to have waited for the slow reader", st.MaxWait)
	}
}

func TestSendGivesUpWhenContextEnds(t *testing.T) {
	c := New(1)
	c.Send(context.Background(), result(1)) // buffer is now full; nobody reads

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := c.Send(ctx, result(2))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if st := c.Stats(); st.Dropped != 1 || st.Sent != 1 {
		t.Errorf("Stats = %+v, want Sent 1 and Dropped 1 (the loss is counted)", st)
	}
}

func TestSendUsesFreeSpaceEvenIfContextEnded(t *testing.T) {
	c := New(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Send(ctx, result(1)); err != nil {
		t.Errorf("err = %v, want nil: there was space, so nothing had to be dropped", err)
	}
}

func TestForwardClosesAfterInput(t *testing.T) {
	in := make(chan worker.JobResult, 3)
	for i := 1; i <= 3; i++ {
		in <- result(i)
	}
	close(in)

	c := New(1)
	go Forward(context.Background(), in, c)

	n := 0
	for range c.Records() { // ends only because Forward closes c
		n++
	}
	if n != 3 {
		t.Errorf("got %d records, want 3", n)
	}
}
