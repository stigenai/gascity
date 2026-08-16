package omnigent

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventStreamHighVolumeBackpressureIsOrderedAndBounded(t *testing.T) {
	const events = 20_000
	stream := newStreamLoad(events)
	started := time.Now()
	seen := 0
	err := stream.Consume(context.Background(), func(event StreamEvent) error {
		if event.Delta != strconv.Itoa(seen) {
			return fmt.Errorf("event %d delta=%q", seen, event.Delta)
		}
		seen++
		if seen%257 == 0 {
			runtime.Gosched() // exercise producer/consumer backpressure without sleeps
		}
		return nil
	})
	if err != nil || seen != events {
		t.Fatalf("ConsumeStream events=%d error=%v", seen, err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("offline stream guard took %s", elapsed)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReconnectStormCleansConnectionsAndGoroutines(t *testing.T) {
	const streams = 32
	beforeGoroutines := runtime.NumGoroutine()
	beforeFDs := openFDCount()
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := 0
			stream := newStreamLoad(128)
			err := stream.Consume(context.Background(), func(StreamEvent) error {
				seen++
				return nil
			})
			_ = stream.Close()
			if err == nil && seen != 128 {
				err = fmt.Errorf("events=%d", seen)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > beforeGoroutines+6 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if after := runtime.NumGoroutine(); after > beforeGoroutines+6 {
		t.Fatalf("goroutines before=%d after=%d", beforeGoroutines, after)
	}
	if afterFDs := openFDCount(); beforeFDs >= 0 && afterFDs > beforeFDs+4 {
		t.Fatalf("file descriptors before=%d after=%d", beforeFDs, afterFDs)
	}
}

func TestOmnigentOptInConcurrencySoak(t *testing.T) {
	if os.Getenv("GC_OMNIGENT_SOAK") != "1" {
		t.Skip("set GC_OMNIGENT_SOAK=1 for the long offline reconnect/backpressure soak")
	}
	for round := 0; round < 100; round++ {
		stream := newStreamLoad(1_000)
		if err := stream.Consume(context.Background(), func(StreamEvent) error { return nil }); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		_ = stream.Close()
	}
}

func BenchmarkConsumeSSEData(b *testing.B) {
	payload := `{"type":"content.delta","conversation_id":"conv_bench","sequence_number":42,"delta":"portable output"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := consumeSSEData(payload, func(StreamEvent) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRedactingWriter(b *testing.B) {
	payload := []byte("backend token=SENTINEL-BENCHMARK-SECRET completed\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		writer := newRedactingWriter(ioDiscard{})
		if _, err := writer.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }

func newStreamLoad(events int) *EventStream {
	var body strings.Builder
	for i := 0; i < events; i++ {
		_, _ = fmt.Fprintf(&body, "data: {\"type\":\"content.delta\",\"conversation_id\":\"conv_load\",\"sequence_number\":%d,\"delta\":%q}\n\n", i, strconv.Itoa(i))
	}
	_, _ = fmt.Fprint(&body, "data: [DONE]\n\n")
	return &EventStream{body: io.NopCloser(strings.NewReader(body.String()))}
}

func openFDCount() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}
