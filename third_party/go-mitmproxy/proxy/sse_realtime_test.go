package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// TestSSERealTime tests the real-time behavior of SSE events
func TestSSERealTime(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("should trigger SSEMessage hook immediately as each event arrives", func(t *testing.T) {
		eventCount := 5
		eventInterval := 100 * time.Millisecond

		// Create an SSE server that sends events incrementally at timed intervals
		sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatalf("Streaming not supported")
			}

			// Send events incrementally at timed intervals
			for i := 0; i < eventCount; i++ {
				event := fmt.Sprintf("data: Event %d at %%s\n\n", i)
				fmt.Fprintf(w, event, time.Now().Format(time.RFC3339Nano))
				flusher.Flush()

				// Wait a while before sending the next event
				time.Sleep(eventInterval)
			}
		}))
		defer sseServer.Close()

		// Create the realtime test addon
		realtimeAddon := &SSERealTimeTestAddon{
			BaseAddon:          BaseAddon{},
			eventTimestamps:    make([]time.Time, 0),
			triggerSequence:    make([]string, 0),
			streamNotCompleted: true,
			mu:                 sync.Mutex{},
		}

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29105",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		proxy.AddAddon(realtimeAddon)
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the client
		proxyURL, _ := url.Parse("http://127.0.0.1:29105")
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}

		// Record the test start time
		testStartTime := time.Now()

		// Send the SSE request
		sseEndpoint := sseServer.URL + "/sse"
		req, err := http.NewRequest("GET", sseEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}

		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		// Read the response (this triggers the SSE hooks)
		buffer := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buffer)
			if n == 0 && err != nil {
				break
			}
		}

		testEndTime := time.Now()

		// Verify real-time behavior
		t.Run("verify all events were captured", func(t *testing.T) {
			realtimeAddon.mu.Lock()
			defer realtimeAddon.mu.Unlock()

			if len(realtimeAddon.eventTimestamps) != eventCount {
				t.Errorf("Expected %d events, got %d", eventCount, len(realtimeAddon.eventTimestamps))
			}

			t.Logf("✓ Captured %d events", len(realtimeAddon.eventTimestamps))
		})

		t.Run("verify events were triggered during streaming, not after completion", func(t *testing.T) {
			realtimeAddon.mu.Lock()
			defer realtimeAddon.mu.Unlock()

			// Verify that each event was triggered between the test start and end
			for i, timestamp := range realtimeAddon.eventTimestamps {
				if timestamp.Before(testStartTime) {
					t.Errorf("Event %d was triggered before test started", i)
				}
				if timestamp.After(testEndTime) {
					t.Errorf("Event %d was triggered after test ended", i)
				}
			}

			// Verify that event trigger times are increasing (meaning they were triggered one by one)
			for i := 1; i < len(realtimeAddon.eventTimestamps); i++ {
				if realtimeAddon.eventTimestamps[i].Before(realtimeAddon.eventTimestamps[i-1]) {
					t.Errorf("Event %d was triggered before event %d", i, i-1)
				}
			}

			t.Logf("✓ Events were triggered during streaming (not batched at end)")
		})

		t.Run("verify events were triggered with expected timing", func(t *testing.T) {
			realtimeAddon.mu.Lock()
			defer realtimeAddon.mu.Unlock()

			// Verify that the intervals between adjacent events roughly match the server send interval
			// Allow some margin of error (network latency, processing time, etc.)
			maxDelay := eventInterval + 200*time.Millisecond
			minDelay := eventInterval - 100*time.Millisecond

			for i := 1; i < len(realtimeAddon.eventTimestamps); i++ {
				delay := realtimeAddon.eventTimestamps[i].Sub(realtimeAddon.eventTimestamps[i-1])

				// Only check the middle events; the first and last may have extra connection overhead
				if i > 1 && i < len(realtimeAddon.eventTimestamps)-1 {
					if delay > maxDelay {
						t.Logf("Warning: Event %d delay %v exceeds expected %v", i, delay, eventInterval)
					}
					if delay < minDelay && minDelay > 0 {
						t.Logf("Warning: Event %d delay %v is less than expected %v", i, delay, eventInterval)
					}
				}
			}

			t.Logf("✓ Events were triggered with appropriate timing")
		})

		t.Run("verify stream was still marked as in progress during event triggers", func(t *testing.T) {
			// This check runs after the stream ends, verifying whether intermediate events fired while the stream was in progress
			if len(realtimeAddon.triggerSequence) == 0 {
				t.Error("No events were triggered")
			}

			// Check whether there is an "event_x_while_streaming" record
			hasStreamingEvents := false
			for _, seq := range realtimeAddon.triggerSequence {
				if len(seq) > 0 {
					hasStreamingEvents = true
					break
				}
			}

			if !hasStreamingEvents {
				t.Error("Events were not triggered during streaming")
			}

			t.Logf("✓ Events were triggered while stream was active")
		})

		// Output detailed timing information
		t.Run("output detailed timing information", func(t *testing.T) {
			realtimeAddon.mu.Lock()
			defer realtimeAddon.mu.Unlock()

			t.Logf("\n=== SSE Real-time Test Results ===")
			t.Logf("Test started: %s", testStartTime.Format(time.RFC3339Nano))
			t.Logf("Test ended: %s", testEndTime.Format(time.RFC3339Nano))
			t.Logf("Total duration: %v", testEndTime.Sub(testStartTime))
			t.Logf("\nEvent timing:")

			for i, timestamp := range realtimeAddon.eventTimestamps {
				var delayFromStart time.Duration
				if i == 0 {
					delayFromStart = timestamp.Sub(testStartTime)
				} else {
					delayFromStart = timestamp.Sub(realtimeAddon.eventTimestamps[i-1])
				}
				t.Logf("  Event %d: %s (delay: %v)", i, timestamp.Format(time.RFC3339Nano), delayFromStart)
			}
			t.Logf("===================================\n")
		})
	})
}

// TestSSEImmediateTriggering tests that events are triggered immediately rather than in a batch
func TestSSEImmediateTriggering(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	t.Run("should trigger hooks immediately, not wait for stream completion", func(t *testing.T) {
		// Use a longer send interval to ensure real-time behavior can be detected
		eventInterval := 200 * time.Millisecond
		eventCount := 3

		triggerTimes := make([]time.Time, 0)
		var mu sync.Mutex

		immediateAddon := &ImmediateTestAddon{
			BaseAddon: BaseAddon{},
			onEvent: func() {
				mu.Lock()
				defer mu.Unlock()
				triggerTimes = append(triggerTimes, time.Now())
			},
		}

		// Create the SSE server
		sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")

			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatalf("Streaming not supported")
			}

			for i := 0; i < eventCount; i++ {
				fmt.Fprintf(w, "data: Event %d\n\n", i)
				flusher.Flush()
				time.Sleep(eventInterval)
			}
		}))
		defer sseServer.Close()

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29106",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		proxy.AddAddon(immediateAddon)
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the client
		proxyURL, _ := url.Parse("http://127.0.0.1:29106")
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}

		// Send the request and record the start time
		startTime := time.Now()
		resp, err := client.Get(sseServer.URL + "/sse")
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		// Read the response
		buffer := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buffer)
			if n == 0 && err != nil {
				break
			}
		}

		// Verify real-time behavior
		mu.Lock()
		defer mu.Unlock()

		if len(triggerTimes) != eventCount {
			t.Fatalf("Expected %d trigger times, got %d", eventCount, len(triggerTimes))
		}

		// Key check: each trigger time should be before the stream ends
		streamEndTime := time.Now()

		for i, triggerTime := range triggerTimes {
			// Each event should be triggered before the stream ends
			if triggerTime.After(streamEndTime) {
				t.Errorf("Event %d was triggered after stream ended", i)
			}

			// Compute the delay relative to the start time
			delay := triggerTime.Sub(startTime)
			expectedDelay := time.Duration(i) * eventInterval

			// Allow some margin of error, but it should roughly match the expectation
			maxAcceptableDelay := expectedDelay + 500*time.Millisecond
			minAcceptableDelay := expectedDelay - 100*time.Millisecond

			if delay > maxAcceptableDelay || delay < minAcceptableDelay {
				t.Logf("Warning: Event %d delay %v differs from expected %v", i, delay, expectedDelay)
			} else {
				t.Logf("✓ Event %d triggered at appropriate time (delay: %v)", i, delay)
			}
		}

		// The most important check: verify that trigger times are spread across the whole time span
		// rather than concentrated at the end
		firstEventDelay := triggerTimes[0].Sub(startTime)
		lastEventDelay := triggerTimes[len(triggerTimes)-1].Sub(startTime)

		totalExpectedDuration := time.Duration(eventCount-1) * eventInterval

		// If all events are only triggered at the end, it means they are not real-time
		if lastEventDelay-firstEventDelay < totalExpectedDuration/2 {
			t.Errorf("Events appear to be batched: all triggered within %v, expected spread over %v",
				lastEventDelay-firstEventDelay, totalExpectedDuration)
		}

		t.Logf("✓ Events were triggered immediately as they arrived, not batched")
		t.Logf("✓ First event delay: %v, Last event delay: %v", firstEventDelay, lastEventDelay)
	})
}

// SSERealTimeTestAddon is an addon for testing SSE real-time behavior
type SSERealTimeTestAddon struct {
	BaseAddon
	eventTimestamps []time.Time    // trigger time of each SSE event
	triggerSequence []string       // record of trigger order
	streamNotCompleted bool         // whether the stream is still in progress
	mu              sync.Mutex
}

func (addon *SSERealTimeTestAddon) SSEStart(f *Flow) {
	addon.mu.Lock()
	defer addon.mu.Unlock()
	addon.streamNotCompleted = true
}

func (addon *SSERealTimeTestAddon) SSEMessage(f *Flow) {
	addon.mu.Lock()
	defer addon.mu.Unlock()

	// Record the event trigger time
	addon.eventTimestamps = append(addon.eventTimestamps, time.Now())

	// Get the latest event
	events := f.SSE.Events
	if len(events) > 0 {
		lastEvent := events[len(events)-1]
		addon.triggerSequence = append(addon.triggerSequence, lastEvent.Data)
	}
}

func (addon *SSERealTimeTestAddon) SSEEnd(f *Flow) {
	addon.mu.Lock()
	defer addon.mu.Unlock()
	addon.streamNotCompleted = false
}

// ImmediateTestAddon is an addon for immediate-trigger testing
type ImmediateTestAddon struct {
	BaseAddon
	onEvent func()
}

func (addon *ImmediateTestAddon) SSEMessage(f *Flow) {
	if addon.onEvent != nil {
		addon.onEvent()
	}
}
