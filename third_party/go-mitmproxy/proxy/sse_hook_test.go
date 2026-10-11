package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// TestSSEHook tests whether the SSE hooks are triggered correctly
func TestSSEHook(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	t.Run("should trigger SSEStart, SSEMessage and SSEEnd hooks", func(t *testing.T) {
		eventCount := 3

		// Create the SSE server
		sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatalf("Streaming not supported")
			}

			// Send multiple events
			for i := 0; i < eventCount; i++ {
				event := fmt.Sprintf("data: Test Event %d\n\n", i)
				fmt.Fprint(w, event)
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
			}
		}))
		defer sseServer.Close()

		// Create a test addon to capture hook invocations
		testAddon := &SSEHookTestAddon{
			BaseAddon: BaseAddon{},
			started:   false,
			ended:     false,
			eventData: make([]string, 0),
		}

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29103",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		proxy.AddAddon(testAddon)
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the client
		proxyURL, _ := url.Parse("http://127.0.0.1:29103")
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}

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

		// Read the response to trigger the data stream
		buffer := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(buffer)
			if n == 0 && err != nil {
				break
			}
		}

		// Verify that the hooks were triggered
		if !testAddon.started {
			t.Error("SSEStart hook was not triggered")
		}

		if !testAddon.ended {
			t.Error("SSEEnd hook was not triggered")
		}

		if len(testAddon.eventData) != eventCount {
			t.Errorf("Expected %d SSEMessage hooks, got %d", eventCount, len(testAddon.eventData))
		}

		// Verify the event contents
		for i := 0; i < eventCount; i++ {
			expectedData := fmt.Sprintf("Test Event %d", i)
			if testAddon.eventData[i] != expectedData {
				t.Errorf("Event %d: expected data '%s', got '%s'", i, expectedData, testAddon.eventData[i])
			}
		}

		t.Logf("Successfully captured %d SSE events", len(testAddon.eventData))
	})
}

// TestSSEEventParsing tests SSE event parsing
func TestSSEEventParsing(t *testing.T) {
	t.Run("should parse SSE event fields correctly", func(t *testing.T) {
		// Create an SSE server that sends events in various formats
		sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatalf("Streaming not supported")
			}

			// Send an event with an id
			fmt.Fprint(w, "id: 123\ndata: Event with ID\n\n")
			flusher.Flush()

			// Send an event with an event type
			fmt.Fprint(w, "event: custom\ndata: Custom event\n\n")
			flusher.Flush()

			// Send an event with a retry
			fmt.Fprint(w, "retry: 5000\ndata: Event with retry\n\n")
			flusher.Flush()

			// Send multi-line data
			fmt.Fprint(w, "data: line 1\ndata: line 2\n\n")
			flusher.Flush()
		}))
		defer sseServer.Close()

		// Create the test addon
		testAddon := &SSEEventTestAddon{
			BaseAddon: BaseAddon{},
			events:    make([]*SSEEvent, 0),
		}

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29104",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		proxy.AddAddon(testAddon)
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the client
		proxyURL, _ := url.Parse("http://127.0.0.1:29104")
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}

		// Send the request
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

		// Verify the events
		if len(testAddon.events) != 4 {
			t.Errorf("Expected 4 events, got %d", len(testAddon.events))
		}

		// Verify the first event (with an ID)
		if testAddon.events[0].ID != "123" {
			t.Errorf("Event 0: expected ID '123', got '%s'", testAddon.events[0].ID)
		}
		if testAddon.events[0].Data != "Event with ID" {
			t.Errorf("Event 0: expected data 'Event with ID', got '%s'", testAddon.events[0].Data)
		}

		// Verify the second event (with an event type)
		if testAddon.events[1].Event != "custom" {
			t.Errorf("Event 1: expected event type 'custom', got '%s'", testAddon.events[1].Event)
		}
		if testAddon.events[1].Data != "Custom event" {
			t.Errorf("Event 1: expected data 'Custom event', got '%s'", testAddon.events[1].Data)
		}

		// Verify the third event (with a retry)
		if testAddon.events[2].Retry != 5000 {
			t.Errorf("Event 2: expected retry 5000, got %d", testAddon.events[2].Retry)
		}

		// Verify the fourth event (multi-line data)
		expectedMultiLine := "line 1\nline 2"
		if testAddon.events[3].Data != expectedMultiLine {
			t.Errorf("Event 3: expected data '%s', got '%s'", expectedMultiLine, testAddon.events[3].Data)
		}

		t.Logf("Successfully parsed %d SSE events with various formats", len(testAddon.events))
	})
}

// SSEHookTestAddon tests SSE hook triggering
type SSEHookTestAddon struct {
	BaseAddon
	started   bool
	ended     bool
	eventData []string
}

func (addon *SSEHookTestAddon) SSEStart(f *Flow) {
	addon.started = true
}

func (addon *SSEHookTestAddon) SSEMessage(f *Flow) {
	// Get the latest SSE event
	events := f.SSE.Events
	if len(events) == 0 {
		return
	}
	lastEvent := events[len(events)-1]
	addon.eventData = append(addon.eventData, lastEvent.Data)
}

func (addon *SSEHookTestAddon) SSEEnd(f *Flow) {
	addon.ended = true
}

// SSEEventTestAddon tests SSE event parsing
type SSEEventTestAddon struct {
	BaseAddon
	events []*SSEEvent
}

func (addon *SSEEventTestAddon) SSEMessage(f *Flow) {
	// Get the latest SSE event
	events := f.SSE.Events
	if len(events) == 0 {
		return
	}
	lastEvent := events[len(events)-1]
	addon.events = append(addon.events, lastEvent)
}
