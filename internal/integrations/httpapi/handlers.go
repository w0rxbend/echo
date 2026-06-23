package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/matrix"
)

type notifyRequest struct {
	Title     string            `json:"title,omitempty"`
	Message   string            `json:"message,omitempty"`
	Level     string            `json:"level,omitempty"`
	Animation string            `json:"animation,omitempty"`
	Duration  string            `json:"duration,omitempty"`
	Priority  int               `json:"priority,omitempty"`
	Restore   string            `json:"restore,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

type playRequest struct {
	Animation string            `json:"animation"`
	Duration  string            `json:"duration,omitempty"`
	Priority  int               `json:"priority,omitempty"`
	Restore   string            `json:"restore,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

type brightnessRequest struct {
	Value uint8 `json:"value"`
}

type colorRequest struct {
	R byte `json:"r"`
	G byte `json:"g"`
	B byte `json:"b"`
}

type presetRequest struct {
	EffectID byte          `json:"effect_id"`
	Interval string        `json:"interval,omitempty"`
	Color    *colorRequest `json:"color,omitempty"`
	R        byte          `json:"r,omitempty"`
	G        byte          `json:"g,omitempty"`
	B        byte          `json:"b,omitempty"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var event events.Event
	if err := decodeJSON(r, &event); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if event.Source == "" {
		event.Source = events.SourceExternal
	}
	if event.Type == "" {
		writeError(w, http.StatusBadRequest, "type is required")
		return
	}
	if err := s.validateEventOverrides(event.Attributes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if event.ID == "" {
		event.ID = s.nextID("event")
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	if err := s.bus.Publish(r.Context(), event); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"event_id": event.ID})
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	var req notifyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := parseOptionalDuration(req.Duration); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Restore != "" && !validRestorePolicy(animations.RestorePolicy(req.Restore)) {
		writeError(w, http.StatusBadRequest, "invalid restore policy")
		return
	}
	if req.Animation != "" {
		if err := s.validatePlayableAnimation(req.Animation); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	attrs := make(map[string]string)
	addAttr(attrs, "title", req.Title)
	addAttr(attrs, "level", req.Level)
	addAttr(attrs, "animation", req.Animation)
	addAttr(attrs, "duration", req.Duration)
	addAttr(attrs, "restore", req.Restore)
	for key, value := range req.Params {
		addAttr(attrs, "param."+key, value)
	}

	event := events.Event{
		ID:         s.nextID("notify"),
		Source:     events.SourceHTTP,
		Type:       "notify",
		Text:       req.Message,
		Priority:   req.Priority,
		ReceivedAt: time.Now().UTC(),
		Attributes: attrs,
	}
	if err := s.bus.Publish(r.Context(), event); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"event_id": event.ID})
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var req playRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Animation == "" {
		writeError(w, http.StatusBadRequest, "animation is required")
		return
	}
	if err := s.validatePlayableAnimation(req.Animation); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	duration, err := parseOptionalDuration(req.Duration)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	restore := animations.RestorePolicy(req.Restore)
	if restore == "" {
		restore = animations.RestoreBackground
	}
	if !validRestorePolicy(restore) {
		writeError(w, http.StatusBadRequest, "invalid restore policy")
		return
	}

	request := animations.AnimationRequest{
		ID:            s.nextID("play"),
		AnimationID:   req.Animation,
		Params:        animations.Params(req.Params),
		Priority:      req.Priority,
		MaxDuration:   duration,
		InterruptMode: animations.InterruptNone,
		RestorePolicy: restore,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.scheduler.EnqueueRequest(r.Context(), request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"request_id": request.ID})
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"depth": s.scheduler.QueueLen(),
		"state": s.scheduler.State(),
		"items": s.scheduler.QueueSnapshot(),
	})
}

func (s *Server) handleQueueClear(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"cleared": s.scheduler.ClearQueue()})
}

func (s *Server) handleAnimations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"animations": s.registry.RenderableIDs()})
}

func (s *Server) handleAnimationCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := s.registry.Catalog()
	entries := make([]animationCatalogEntry, 0, len(catalog))
	for _, entry := range catalog {
		var interval *string
		if entry.Interval != nil {
			formatted := entry.Interval.String()
			interval = &formatted
		}
		entries = append(entries, animationCatalogEntry{
			ID:       entry.ID,
			Kind:     entry.Kind,
			Playable: entry.Playable,
			EffectID: entry.EffectID,
			Interval: interval,
			Color:    entry.Color,
		})
	}
	writeJSON(w, http.StatusOK, animationCatalogResponse{Animations: entries})
}

func (s *Server) validatePlayableAnimation(id string) error {
	entry, ok := s.registry.Entry(id)
	if !ok {
		return fmt.Errorf("unknown animation %q", id)
	}
	if entry.Animation == nil {
		return fmt.Errorf("animation %q is not renderable/playable", id)
	}
	return nil
}

type animationCatalogResponse struct {
	Animations []animationCatalogEntry `json:"animations"`
}

type animationCatalogEntry struct {
	ID       string                `json:"id"`
	Kind     animations.PublicKind `json:"kind"`
	Playable bool                  `json:"playable"`
	EffectID *byte                 `json:"effect_id,omitempty"`
	Interval *string               `json:"interval,omitempty"`
	Color    *animations.RGB       `json:"color,omitempty"`
}

func (s *Server) validateEventOverrides(attrs map[string]string) error {
	if animationID := attrs["animation"]; animationID != "" {
		if err := s.validatePlayableAnimation(animationID); err != nil {
			return err
		}
	}
	if restore := attrs["restore"]; restore != "" && !validRestorePolicy(animations.RestorePolicy(restore)) {
		return errors.New("invalid restore policy")
	}
	if _, err := parseOptionalDuration(attrs["duration"]); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleMatrixClear(w http.ResponseWriter, r *http.Request) {
	if err := s.scheduler.Clear(r.Context()); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMatrixBrightness(w http.ResponseWriter, r *http.Request) {
	var req brightnessRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.scheduler.SetBrightness(r.Context(), req.Value); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMatrixPreset(w http.ResponseWriter, r *http.Request) {
	var req presetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	interval, err := parseOptionalDuration(req.Interval)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	color := matrix.RGB{R: req.R, G: req.G, B: req.B}
	if req.Color != nil {
		color = matrix.RGB{R: req.Color.R, G: req.Color.G, B: req.Color.B}
	}
	if err := s.scheduler.SetPreset(r.Context(), req.EffectID, interval, color); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMatrixFill(w http.ResponseWriter, r *http.Request) {
	var req colorRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.scheduler.Fill(r.Context(), matrix.RGB{R: req.R, G: req.G, B: req.B}); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeMatrixControlError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, matrixControlStatus(r, err), err.Error())
}

func matrixControlStatus(r *http.Request, err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, matrix.ErrInvalidControl) ||
		errors.Is(err, matrix.ErrInvalidDuration) ||
		errors.Is(err, matrix.ErrPayloadTooLarge) {
		return http.StatusBadRequest
	}
	if errors.Is(err, matrix.ErrProtocol) {
		return http.StatusBadGateway
	}
	var statusErr *matrix.StatusError
	if errors.As(err, &statusErr) {
		return http.StatusBadGateway
	}
	if errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == context.DeadlineExceeded {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, matrix.ErrSchedulerStopped) ||
		errors.Is(err, matrix.ErrControlQueueCleared) ||
		errors.Is(err, matrix.ErrControlDropped) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		matrix.IsRetryableError(r.Context(), err) {
		return http.StatusServiceUnavailable
	}
	return http.StatusServiceUnavailable
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) == nil {
		return errors.New("request body must contain a single JSON value")
	}
	return nil
}

func parseOptionalDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %w", err)
	}
	if duration < 0 {
		return 0, errors.New("duration cannot be negative")
	}
	return duration, nil
}

func validRestorePolicy(policy animations.RestorePolicy) bool {
	switch policy {
	case animations.RestoreClear, animations.RestoreBlank, animations.RestorePreviousFrame, animations.RestoreBackground, animations.RestoreLeave:
		return true
	default:
		return false
	}
}

func addAttr(attrs map[string]string, key, value string) {
	if value != "" {
		attrs[key] = value
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
