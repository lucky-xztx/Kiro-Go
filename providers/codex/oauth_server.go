package codex

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// CallbackServer is a minimal HTTP server that listens for an OAuth callback
// on a local port and captures the authorization code.
type CallbackServer struct {
	port     int
	server   *http.Server
	code     string
	state    string
	done     chan struct{}
	mu       sync.Mutex
	received bool
}

// NewCallbackServer creates a callback server that will listen on the given port.
func NewCallbackServer(port int) *CallbackServer {
	return &CallbackServer{port: port, done: make(chan struct{})}
}

// Start starts the HTTP server in the background.
func (cs *CallbackServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", cs.handleCallback)

	cs.server = &http.Server{
		Handler: mux,
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cs.port))
	if err != nil {
		return fmt.Errorf("codex callback server listen: %w", err)
	}

	go cs.server.Serve(ln)
	return nil
}

func (cs *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	cs.mu.Lock()
	cs.code = code
	cs.state = state
	cs.received = true
	cs.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html><html><body style="display:flex;justify-content:center;align-items:center;height:100vh;font-family:system-ui,sans-serif;background:#1a1a2e;color:#fff"><div style="text-align:center"><h1>✓ Authentication Successful</h1><p>You can close this tab now.</p></div></body></html>`))

	close(cs.done)
}

// WaitForCallback blocks until a callback is received or timeout elapses.
func (cs *CallbackServer) WaitForCallback(timeout time.Duration) (code, state string, err error) {
	select {
	case <-cs.done:
		cs.mu.Lock()
		defer cs.mu.Unlock()
		if !cs.received {
			return "", "", fmt.Errorf("callback received but no code")
		}
		return cs.code, cs.state, nil
	case <-time.After(timeout):
		return "", "", fmt.Errorf("timed out waiting for OAuth callback (%v)", timeout)
	}
}

// Close shuts down the callback server.
func (cs *CallbackServer) Close() {
	if cs.server != nil {
		cs.server.Close()
	}
}
