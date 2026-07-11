package server

import "miroxy/internal/config"

// Proxy lifecycle helpers — used by the serve loop (cmd/miroxy/serve.go)
// and the admin stop/start endpoints.

// StopProxyCh returns the channel the serve loop selects on to stop the proxy.
func (s *Server) StopProxyCh() <-chan struct{} { return s.stopProxyCh }

// StartProxyCh returns the channel the serve loop selects on to restart the proxy.
func (s *Server) StartProxyCh() <-chan struct{} { return s.startProxyCh }

// SetProxyRunning records whether the proxy listener is currently active.
func (s *Server) SetProxyRunning(v bool) { s.proxyRunning.Store(v) }

// IsProxyRunning reports whether the proxy listener is currently active.
func (s *Server) IsProxyRunning() bool { return s.proxyRunning.Load() }

// SignalStop sends a non-blocking stop signal to the serve loop.
func (s *Server) SignalStop() {
	select {
	case s.stopProxyCh <- struct{}{}:
	default:
	}
}

// SignalStart sends a non-blocking start signal to the serve loop.
func (s *Server) SignalStart() {
	select {
	case s.startProxyCh <- struct{}{}:
	default:
	}
}

// CurrentConfig returns the currently loaded config (used by the serve loop on restart).
func (s *Server) CurrentConfig() *config.Config { return s.cfg.Load() }
