package server

import "net/http"

// CORSConfig configures cross-origin resource sharing. AllowedOrigins is an
// exact-match allowlist; an entry of "*" allows any origin. Empty disables CORS.
type CORSConfig struct {
	AllowedOrigins []string
}

func (c *CORSConfig) allows(origin string) bool {
	for _, o := range c.AllowedOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

// withCORS wraps h with CORS handling. Returns h unchanged when CORS is nil or has no
// origins (byte-identical pass-through). Must be the OUTERMOST middleware so preflight
// OPTIONS short-circuits before auth/rate-limit and CORS headers ride on 401/429.
func (s *Server) withCORS(h http.Handler) http.Handler {
	cfg := s.opts.CORS
	if cfg == nil || len(cfg.AllowedOrigins) == 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && cfg.allows(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
