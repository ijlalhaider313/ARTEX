package proxy

import (
	"bufio"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

var wsHandshakeHeaders = map[string]struct{}{
	"Upgrade":                  {},
	"Connection":               {},
	"Sec-Websocket-Key":        {},
	"Sec-Websocket-Version":    {},
	"Sec-Websocket-Extensions": {},
	"Sec-Websocket-Protocol":   {},
}

func cloneHeaderWithoutWSHandshake(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vals := range h {
		if _, skip := wsHandshakeHeaders[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		if strings.EqualFold(k, "Host") {
			continue
		}
		cloned := make([]string, len(vals))
		copy(cloned, vals)
		out[k] = cloned
	}
	return out
}

type webSocketHandler struct {
	proxy *Proxy
}

func newWebSocketHandler(proxy *Proxy) *webSocketHandler {
	return &webSocketHandler{proxy: proxy}
}

// connResponseWriter is a custom ResponseWriter that wraps net.Conn
// so that websocket.Upgrader can upgrade the connection
type connResponseWriter struct {
	conn        net.Conn
	header      http.Header
	statusCode  int
	wroteHeader bool
}

func newConnResponseWriter(conn net.Conn) *connResponseWriter {
	return &connResponseWriter{
		conn:   conn,
		header: make(http.Header),
	}
}

func (w *connResponseWriter) Header() http.Header {
	return w.header
}

func (w *connResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(data)
}

func (w *connResponseWriter) WriteHeader(statusCode int) {
	w.wroteHeader = true
	w.statusCode = statusCode
}

// Hijack hijacks the connection, returning the underlying net.Conn and bufio.ReadWriter
// websocket.Upgrader.Upgrade() needs to call this method
func (w *connResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	buf := bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn))
	return w.conn, buf, nil
}

// handle handles a WebSocket connection
// serverConn: connection to the server (TCP connection already established)
// clientConn: connection to the client (CONNECT already completed, the client has sent the WebSocket handshake request)
func (h *webSocketHandler) handle(serverConn, clientConn net.Conn, f *Flow) error {
	// Step 1: read the client handshake request
	buf := bufio.NewReader(clientConn)
	clientReq, err := http.ReadRequest(buf)
	if err != nil {
		log.Errorf("Failed to read client handshake: %v", err)
		return err
	}

	log.Debugf("Client WebSocket handshake: %s %s", clientReq.Method, clientReq.URL.Path)

	// Step 2: use Dialer to connect to the server
	dialer := &websocket.Dialer{
		NetDial: func(network, addr string) (net.Conn, error) {
			return serverConn, nil
		},
		// Use the existing connection, no need to dial again
		HandshakeTimeout: 0,
	}

	serverURL := "ws://" + clientReq.Host + clientReq.URL.RequestURI()
	log.Debugf("Connecting to server: %s", serverURL)

	// Dialer automatically adds all required WebSocket handshake headers
	// We do not pass clientReq.Header, to avoid duplicate header errors
	serverWS, _, err := dialer.Dial(serverURL, nil)
	if err != nil {
		log.Errorf("Failed to dial server: %v", err)
		return err
	}
	defer serverWS.Close()

	log.Debugf("Server WebSocket connected, subprotocol: %s", serverWS.Subprotocol())

	// Step 3: use Upgrader to upgrade the client connection
	respWriter := newConnResponseWriter(clientConn)

	upgrader := &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true // always allow in proxy mode
		},
	}

	clientWS, err := upgrader.Upgrade(respWriter, clientReq, nil)
	if err != nil {
		log.Errorf("Failed to upgrade client connection: %v", err)
		return err
	}
	defer clientWS.Close()

	log.Debugf("Client WebSocket upgraded successfully")

	wsData := newWebSocketData()
	f.WebScoket = wsData

	for _, addon := range h.proxy.Addons {
		addon.WebSocketStart(f)
	}

	// Step 4: forward messages in both directions
	return h.forwardMessages(clientWS, serverWS, f)
}

// forwardMessages forwards WebSocket messages in both directions
func (h *webSocketHandler) forwardMessages(clientWS, serverWS *websocket.Conn, f *Flow) error {
	defer func() {
		for _, addon := range h.proxy.Addons {
			addon.WebSocketEnd(f)
		}
	}()

	errChan := make(chan error, 2)

	// Client -> Server
	go func() {
		defer func() {
			// Gracefully close the server connection
			serverWS.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			serverWS.Close()
		}()

		for {
			msgType, msg, err := clientWS.ReadMessage()
			if err != nil {
				// Check whether this is a normal close
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					errChan <- nil // normal close, do not return an error
					return
				}
				errChan <- err
				return
			}

			f.WebScoket.addMessage(msgType, msg, true)
			for _, addon := range h.proxy.Addons {
				addon.WebSocketMessage(f)
			}

			if err := serverWS.WriteMessage(msgType, msg); err != nil {
				log.Errorf("Client -> Server: Write error: %v", err)
				errChan <- err
				return
			}
		}
	}()

	// Server -> Client
	go func() {
		defer func() {
			// Gracefully close the client connection
			clientWS.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			clientWS.Close()
		}()

		for {
			msgType, msg, err := serverWS.ReadMessage()
			if err != nil {
				// Check whether this is a normal close
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					errChan <- nil // normal close, do not return an error
					return
				}
				errChan <- err
				return
			}

			f.WebScoket.addMessage(msgType, msg, false)
			for _, addon := range h.proxy.Addons {
				addon.WebSocketMessage(f)
			}

			if err := clientWS.WriteMessage(msgType, msg); err != nil {
				log.Errorf("Server -> Client: Write error: %v", err)
				errChan <- err
				return
			}
		}
	}()

	// Wait for either direction to error or close
	err := <-errChan
	// If it is a normal close (nil), return nil
	return err
}

func (h *webSocketHandler) handleWSS(res http.ResponseWriter, req *http.Request) error {
	// Fix the WebSocket URL to ensure it contains the full scheme and host
	serverURL := "wss://" + req.Host + req.URL.RequestURI()
	log.Debugf("Connecting to WSS server: %s", serverURL)
	if parsedURL, err := url.Parse(serverURL); err == nil {
		req.URL = parsedURL
	}

	connCtx := req.Context().Value(connContextKey).(*ConnContext)

	// Step 1: get the upstream connection
	plainConn, err := h.proxy.getUpstreamConn(req.Context(), req)
	if err != nil {
		log.Errorf("Failed to get upstream connection: %v", err)
		return err
	}

	// Step 2: create and initialize ServerConn
	serverConn := newServerConn()
	serverConn.Address = req.Host
	serverConn.Conn = &wrapServerConn{
		Conn:    plainConn,
		proxy:   h.proxy,
		connCtx: connCtx,
	}
	connCtx.ServerConn = serverConn

	// Step 3: call the addon's ServerConnected callback
	for _, addon := range connCtx.proxy.Addons {
		addon.ServerConnected(connCtx)
	}

	// Step 4: create a Dialer using a custom NetDial
	dialer := &websocket.Dialer{
		NetDial: func(network, addr string) (net.Conn, error) {
			return serverConn.Conn, nil
		},
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: h.proxy.Opts.SslInsecure,
		},
	}

	upstreamHeader := cloneHeaderWithoutWSHandshake(req.Header)
	serverWS, _, err := dialer.Dial(serverURL, upstreamHeader)
	if err != nil {
		log.Errorf("Failed to dial WSS server: %v", err)
		return err
	}
	defer serverWS.Close()

	upgrader := &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true // always allow in proxy mode
		},
	}

	clientWS, err := upgrader.Upgrade(res, req, nil)
	if err != nil {
		log.Errorf("Failed to upgrade client connection: %v", err)
		return err
	}
	defer clientWS.Close()

	log.Debugf("Client WSS upgraded successfully")

	wsData := newWebSocketData()
	f := newFlow()
	f.Request = newRequest(req)
	f.ConnContext = connCtx
	f.WebScoket = wsData
	defer f.finish()

	for _, addon := range h.proxy.Addons {
		addon.WebSocketStart(f)
	}

	// Forward messages in both directions
	return h.forwardMessages(clientWS, serverWS, f)
}
