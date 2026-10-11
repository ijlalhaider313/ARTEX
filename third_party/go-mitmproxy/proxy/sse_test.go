package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lqqyt2423/go-mitmproxy/cert"
	log "github.com/sirupsen/logrus"
)

// testSSEServer creates a test SSE server
func testSSEServer(t *testing.T, eventCount int) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		// Set the response headers required for SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Create a flusher to ensure data is sent immediately
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("Streaming not supported")
		}

		// Send multiple events
		for i := 0; i < eventCount; i++ {
			event := fmt.Sprintf("data: Message %d\n\n", i)
			_, err := fmt.Fprint(w, event)
			if err != nil {
				t.Logf("Failed to write event: %v", err)
				return
			}
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	})

	return httptest.NewServer(mux)
}

// testTLSSSEServer creates a test HTTPS SSE server
func testTLSSSEServer(t *testing.T, eventCount int) *testTLSServer {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		// Set the response headers required for SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Create a flusher to ensure data is sent immediately
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("Streaming not supported")
		}

		// Send multiple events
		for i := 0; i < eventCount; i++ {
			event := fmt.Sprintf("data: Secure Message %d\n\n", i)
			_, err := fmt.Fprint(w, event)
			if err != nil {
				t.Logf("Failed to write event: %v", err)
				return
			}
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	})

	// Create the TLS listener
	tlsPlainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	// Generate a self-signed certificate
	ca, err := cert.NewSelfSignCA("")
	if err != nil {
		t.Fatalf("Failed to create CA: %v", err)
	}
	tlsCert, err := ca.GetCert("localhost")
	if err != nil {
		t.Fatalf("Failed to get cert: %v", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
	}

	server := &http.Server{
		Handler:   mux,
		TLSConfig: tlsConfig,
	}

	httpsPort := tlsPlainLn.Addr().(*net.TCPAddr).Port
	tlsLn := tls.NewListener(tlsPlainLn, tlsConfig)

	go server.Serve(tlsLn)

	return &testTLSServer{
		server: server,
		tlsLn:  tlsLn,
		port:   httpsPort,
	}
}

// consumeSSEStream consumes the SSE stream and returns the number of events received
func consumeSSEStream(t *testing.T, body io.Reader) int {
	t.Helper()

	eventCount := 0
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			eventCount++
		}
	}

	if err := scanner.Err(); err != nil {
		t.Logf("Scanner error: %v", err)
	}

	return eventCount
}

// TestSSEProxy tests basic SSE proxy functionality
func TestSSEProxy(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	t.Run("should proxy SSE connection over HTTP", func(t *testing.T) {
		eventCount := 5
		sseServer := testSSEServer(t, eventCount)
		defer sseServer.Close()

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29097",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client
		proxyURL, _ := url.Parse("http://127.0.0.1:29097")
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

		// Set the SSE request headers
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		// Verify the response headers
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream', got '%s'", resp.Header.Get("Content-Type"))
		}

		// Consume the SSE stream and verify the event count
		receivedCount := consumeSSEStream(t, resp.Body)
		if receivedCount != eventCount {
			t.Errorf("Expected %d events, got %d", eventCount, receivedCount)
		}
	})

	t.Run("should proxy SSE connection over HTTPS", func(t *testing.T) {
		eventCount := 5
		sseServer := testTLSSSEServer(t, eventCount)
		defer sseServer.Close()

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr:        ":29098",
			SslInsecure: true,
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client
		proxyURL, _ := url.Parse("http://127.0.0.1:29098")
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}

		// Send the SSE request
		sseEndpoint := "https://" + sseServer.Addr() + "/sse"
		req, err := http.NewRequest("GET", sseEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}

		// Set the SSE request headers
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		// Verify the response headers
		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream', got '%s'", resp.Header.Get("Content-Type"))
		}

		// Consume the SSE stream and verify the event count
		receivedCount := consumeSSEStream(t, resp.Body)
		if receivedCount != eventCount {
			t.Errorf("Expected %d events, got %d", eventCount, receivedCount)
		}
	})
}

// TestSSEWithStreamModifier tests modifying the SSE streaming response
func TestSSEWithStreamModifier(t *testing.T) {
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	t.Run("should modify SSE stream using StreamResponseModifier", func(t *testing.T) {
		eventCount := 3
		sseServer := testSSEServer(t, eventCount)
		defer sseServer.Close()

		// Create a test addon to modify the SSE stream
		testAddon := &SSETestAddon{
			BaseAddon: BaseAddon{},
			events:    make([]string, 0),
		}

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29099",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		proxy.AddAddon(testAddon)
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client
		proxyURL, _ := url.Parse("http://127.0.0.1:29099")
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

		// Read the response
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response: %v", err)
		}

		// Verify that the response contains the modified content
		responseStr := string(body)
		if !strings.Contains(responseStr, "data: Modified Message") {
			t.Error("Expected modified SSE messages, but they were not found")
		}
	})
}

// SSETestAddon is a test SSE addon
type SSETestAddon struct {
	BaseAddon
	events []string
}

// StreamResponseModifier modifies the SSE streaming response
func (addon *SSETestAddon) StreamResponseModifier(f *Flow, in io.Reader) io.Reader {
	return io.MultiReader(
		strings.NewReader("data: Modified Message 0\n\n"),
		in,
	)
}
