package httpapi

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/matrix"
)

type Server struct {
	logger     *slog.Logger
	bus        *events.Bus
	schedulers map[string]*matrix.Scheduler
	registry   *animations.Registry

	adminToken  string
	requireAuth bool
	ids         atomic.Uint64
}

type Options struct {
	Logger        *slog.Logger
	Bus           *events.Bus
	Schedulers    map[string]*matrix.Scheduler
	Registry      *animations.Registry
	ServerAddr    string
	AdminTokenEnv string
}

func New(options Options) (*Server, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	auth, err := ResolveAdminAuth(options.ServerAddr, options.AdminTokenEnv)
	if err != nil {
		return nil, err
	}

	schedulers := options.Schedulers
	if schedulers == nil {
		schedulers = make(map[string]*matrix.Scheduler)
	}

	return &Server{
		logger:      logger,
		bus:         options.Bus,
		schedulers:  schedulers,
		registry:    options.Registry,
		adminToken:  auth.Token,
		requireAuth: auth.Required,
	}, nil
}

type AdminAuth struct {
	Token    string
	Required bool
}

func ResolveAdminAuth(serverAddr, tokenEnv string) (AdminAuth, error) {
	if isLocalBind(serverAddr) {
		return AdminAuth{}, nil
	}
	if strings.TrimSpace(tokenEnv) == "" {
		return AdminAuth{}, fmt.Errorf("server.admin_token_env is required for non-local server bind %q", serverAddr)
	}

	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if token == "" {
		return AdminAuth{}, fmt.Errorf("server.admin_token_env %q is unset or blank for non-local server bind %q", tokenEnv, serverAddr)
	}

	return AdminAuth{Token: token, Required: true}, nil
}

// Router builds the /api/v1 sub-router mounted by the App.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	// Global animation discovery (no device required).
	r.Get("/animations", s.handleAnimations)
	r.Get("/animations/catalog", s.handleAnimationCatalog)

	// Device list.
	r.With(s.adminOnly).Get("/devices", s.handleDeviceList)

	// Per-device routes.
	r.Route("/devices/{device}", func(r chi.Router) {
		r.Use(s.requireKnownDevice)
		r.Post("/events", s.handleEvents)
		r.Post("/notify", s.handleNotify)
		r.With(s.adminOnly).Post("/play", s.handlePlay)
		r.With(s.adminOnly).Get("/queue", s.handleQueue)
		r.With(s.adminOnly).Delete("/queue", s.handleQueueClear)

		// Runtime idle background configuration.
		r.With(s.adminOnly).Get("/background", s.handleGetBackground)
		r.With(s.adminOnly).Put("/background", s.handleSetBackground)

		r.Route("/matrix", func(r chi.Router) {
			r.Use(s.adminOnly)
			r.Post("/clear", s.handleMatrixClear)
			r.Post("/brightness", s.handleMatrixBrightness)
			r.Post("/preset", s.handleMatrixPreset)
			r.Post("/fill", s.handleMatrixFill)
		})
	})

	return r
}

// deviceFromRequest resolves the {device} URL parameter to the corresponding scheduler.
// Returns nil, "" when the parameter is absent or the device is unknown.
func (s *Server) deviceFromRequest(r *http.Request) (*matrix.Scheduler, string) {
	id := chi.URLParam(r, "device")
	if id == "" {
		return nil, ""
	}
	return s.schedulers[id], id
}

// requireKnownDevice is middleware that rejects requests for unknown device IDs.
func (s *Server) requireKnownDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "device")
		if _, ok := s.schedulers[id]; !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("unknown device %q", id))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleDeviceList(w http.ResponseWriter, _ *http.Request) {
	ids := make([]string, 0, len(s.schedulers))
	for id := range s.schedulers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	writeJSON(w, http.StatusOK, map[string]any{"devices": ids})
}

func (s *Server) adminOnly(next http.Handler) http.Handler {
	if !s.requireAuth {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(auth, prefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
			writeError(w, http.StatusForbidden, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) nextID(prefix string) string {
	n := s.ids.Add(1)
	return prefix + "-" + time.Now().UTC().Format("20060102T150405.000000000") + "-" + strconvFormatUint(n)
}

func isLocalBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func strconvFormatUint(v uint64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	return string(buf[i:])
}
