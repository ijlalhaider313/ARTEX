package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lqqyt2423/go-mitmproxy/cert"
	log "github.com/sirupsen/logrus"
)

// testWebSocketServer creates a test WS server
func testWebSocketServer(t *testing.T, handler func(*websocket.Conn)) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("Failed to upgrade connection: %v", err)
			return
		}
		handler(conn)
	})

	return httptest.NewServer(mux)
}

// testTLSWebSocketServer creates a test WSS (TLS WebSocket) server
func testTLSWebSocketServer(t *testing.T, handler func(*websocket.Conn)) *testTLSServer {
	t.Helper()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("Failed to upgrade connection: %v", err)
			return
		}
		handler(conn)
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

// testTLSServer is a test TLS server wrapper
type testTLSServer struct {
	server *http.Server
	tlsLn  net.Listener
	port   int
}

// Close closes the server
func (s *testTLSServer) Close() {
	s.server.Close()
	s.tlsLn.Close()
}

// Addr returns the server address
func (s *testTLSServer) Addr() string {
	return "localhost:" + strconv.Itoa(s.port)
}

// testEchoWebSocketHandler creates an echo handler
func testEchoWebSocketHandler(t *testing.T) func(*websocket.Conn) {
	return func(conn *websocket.Conn) {
		defer conn.Close()

		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					t.Logf("WebSocket read error: %v", err)
				}
				return
			}

			err = conn.WriteMessage(messageType, message)
			if err != nil {
				t.Logf("WebSocket write error: %v", err)
				return
			}
		}
	}
}

// TestWebSocketWSProxy tests basic WS proxy functionality
func TestWebSocketWSProxy(t *testing.T) {
	// Set the log level to Debug so detailed debug information is visible
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	t.Run("should proxy WS connection", func(t *testing.T) {
		// Create a test WS server
		wsServer := testWebSocketServer(t, testEchoWebSocketHandler(t))
		defer wsServer.Close()

		// Parse the WS server address
		wsURL, err := url.Parse(wsServer.URL)
		if err != nil {
			t.Fatalf("Failed to parse WS server URL: %v", err)
		}

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29090",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client
		proxyURL, _ := url.Parse("http://127.0.0.1:29090")
		dialer := &websocket.Dialer{
			Proxy:            http.ProxyURL(proxyURL),
			HandshakeTimeout: time.Second * 5,
		}

		// Connect to the WS server (through the proxy)
		wsEndpoint := "ws://" + wsURL.Host + "/ws"
		conn, resp, err := dialer.Dial(wsEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to dial WS via proxy: %v, response: %v", err, resp)
		}
		defer conn.Close()

		// Send a test message
		testMessage := "Hello, WebSocket!"

		err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}

		_, receivedMsg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		if string(receivedMsg) != testMessage {
			t.Fatalf("Expected message %q, got %q", testMessage, string(receivedMsg))
		}

		// Close the connection
		err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			t.Fatalf("Failed to send close message: %v", err)
		}

		// Wait for the connection to close
		time.Sleep(time.Millisecond * 100)
	})

	t.Run("should proxy WS connection when UpstreamCert false", func(t *testing.T) {
		// Create a test WS server
		wsServer := testWebSocketServer(t, testEchoWebSocketHandler(t))
		defer wsServer.Close()

		// Parse the WS server address
		wsURL, err := url.Parse(wsServer.URL)
		if err != nil {
			t.Fatalf("Failed to parse WS server URL: %v", err)
		}

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr: ":29091",
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		// Add the UpstreamCertAddon and set it to false
		proxy.AddAddon(NewUpstreamCertAddon(false))

		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client
		proxyURL, _ := url.Parse("http://127.0.0.1:29091")
		dialer := &websocket.Dialer{
			Proxy:            http.ProxyURL(proxyURL),
			HandshakeTimeout: time.Second * 5,
		}

		// Connect to the WS server (through the proxy)
		wsEndpoint := "ws://" + wsURL.Host + "/ws"
		conn, resp, err := dialer.Dial(wsEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to dial WS via proxy: %v, response: %v", err, resp)
		}
		defer conn.Close()

		// Send a test message
		testMessage := "Hello, WebSocket with UpstreamCert false!"

		err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}

		_, receivedMsg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		if string(receivedMsg) != testMessage {
			t.Fatalf("Expected message %q, got %q", testMessage, string(receivedMsg))
		}

		// Close the connection
		err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			t.Fatalf("Failed to send close message: %v", err)
		}

		// Wait for the connection to close
		time.Sleep(time.Millisecond * 100)
	})
}

// TestWebSocketWSSProxy tests basic WSS proxy functionality
func TestWebSocketWSSProxy(t *testing.T) {
	// Set the log level to Debug so detailed debug information is visible
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	t.Run("should proxy WSS connection", func(t *testing.T) {
		// Create a test WSS server
		wssServer := testTLSWebSocketServer(t, testEchoWebSocketHandler(t))
		defer wssServer.Close()

		// Start the proxy
		proxy, err := NewProxy(&Options{
			Addr:        ":29092",
			SslInsecure: true,
		})
		if err != nil {
			t.Fatalf("Failed to create proxy: %v", err)
		}
		go proxy.Start()
		defer proxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client
		proxyURL, _ := url.Parse("http://127.0.0.1:29092")
		dialer := &websocket.Dialer{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			HandshakeTimeout: time.Second * 5,
		}

		// Connect to the WSS server (through the proxy)
		wssEndpoint := "wss://" + wssServer.Addr() + "/ws"
		conn, resp, err := dialer.Dial(wssEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to dial WSS via proxy: %v, response: %v", err, resp)
		}
		defer conn.Close()

		// Send a test message
		testMessage := "Hello, Secure WebSocket!"

		err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}

		_, receivedMsg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		if string(receivedMsg) != testMessage {
			t.Fatalf("Expected message %q, got %q", testMessage, string(receivedMsg))
		}

		// Close the connection
		err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			t.Fatalf("Failed to send close message: %v", err)
		}

		// Wait for the connection to close
		time.Sleep(time.Millisecond * 100)
	})
}

// TestWebSocketUpstreamProxy tests WebSocket proxy functionality through an upstream proxy
func TestWebSocketUpstreamProxy(t *testing.T) {
	// Set the log level to Debug so detailed debug information is visible
	log.SetLevel(log.DebugLevel)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})

	t.Run("should proxy WS connection through upstream proxy", func(t *testing.T) {
		// Create a test WS server
		wsServer := testWebSocketServer(t, testEchoWebSocketHandler(t))
		defer wsServer.Close()

		// Parse the WS server address
		wsURL, err := url.Parse(wsServer.URL)
		if err != nil {
			t.Fatalf("Failed to parse WS server URL: %v", err)
		}

		// Start the upstream proxy (second layer)
		upstreamProxy, err := NewProxy(&Options{
			Addr: ":29093",
		})
		if err != nil {
			t.Fatalf("Failed to create upstream proxy: %v", err)
		}
		go upstreamProxy.Start()
		defer upstreamProxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Start the client proxy (first layer), with upstream pointing to the upstream proxy
		clientProxy, err := NewProxy(&Options{
			Addr:     ":29094",
			Upstream: "http://127.0.0.1:29093",
		})
		if err != nil {
			t.Fatalf("Failed to create client proxy: %v", err)
		}
		go clientProxy.Start()
		defer clientProxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client, connecting to the first-layer proxy
		proxyURL, _ := url.Parse("http://127.0.0.1:29094")
		dialer := &websocket.Dialer{
			Proxy:            http.ProxyURL(proxyURL),
			HandshakeTimeout: time.Second * 5,
		}

		// Connect to the WS server (through both proxy layers)
		wsEndpoint := "ws://" + wsURL.Host + "/ws"
		conn, resp, err := dialer.Dial(wsEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to dial WS via proxy chain: %v, response: %v", err, resp)
		}
		defer conn.Close()

		// Send a test message
		testMessage := "Hello, WebSocket through upstream proxy!"

		err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}

		_, receivedMsg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		if string(receivedMsg) != testMessage {
			t.Fatalf("Expected message %q, got %q", testMessage, string(receivedMsg))
		}

		// Close the connection
		err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			t.Fatalf("Failed to send close message: %v", err)
		}

		// Wait for the connection to close
		time.Sleep(time.Millisecond * 100)
	})

	t.Run("should proxy WSS connection through upstream proxy", func(t *testing.T) {
		// Create a test WSS server
		wssServer := testTLSWebSocketServer(t, testEchoWebSocketHandler(t))
		defer wssServer.Close()

		// Start the upstream proxy (second layer)
		upstreamProxy, err := NewProxy(&Options{
			Addr:        ":29095",
			SslInsecure: true,
		})
		if err != nil {
			t.Fatalf("Failed to create upstream proxy: %v", err)
		}
		go upstreamProxy.Start()
		defer upstreamProxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Start the client proxy (first layer), with upstream pointing to the upstream proxy
		clientProxy, err := NewProxy(&Options{
			Addr:        ":29096",
			Upstream:    "http://127.0.0.1:29095",
			SslInsecure: true,
		})
		if err != nil {
			t.Fatalf("Failed to create client proxy: %v", err)
		}
		go clientProxy.Start()
		defer clientProxy.Close()
		time.Sleep(time.Millisecond * 100)

		// Create the proxy client, connecting to the first-layer proxy
		proxyURL, _ := url.Parse("http://127.0.0.1:29096")
		dialer := &websocket.Dialer{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			HandshakeTimeout: time.Second * 5,
		}

		// Connect to the WSS server (through both proxy layers)
		wssEndpoint := "wss://" + wssServer.Addr() + "/ws"
		conn, resp, err := dialer.Dial(wssEndpoint, nil)
		if err != nil {
			t.Fatalf("Failed to dial WSS via proxy chain: %v, response: %v", err, resp)
		}
		defer conn.Close()

		// Send a test message
		testMessage := "Hello, Secure WebSocket through upstream proxy!"

		err = conn.WriteMessage(websocket.TextMessage, []byte(testMessage))
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}

		_, receivedMsg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read message: %v", err)
		}

		if string(receivedMsg) != testMessage {
			t.Fatalf("Expected message %q, got %q", testMessage, string(receivedMsg))
		}

		// Close the connection
		err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			t.Fatalf("Failed to send close message: %v", err)
		}

		// Wait for the connection to close
		time.Sleep(time.Millisecond * 100)
	})
}
