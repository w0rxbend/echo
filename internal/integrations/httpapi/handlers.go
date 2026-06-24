package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/worxbend/echo/internal/animations"
	"github.com/worxbend/echo/internal/events"
	"github.com/worxbend/echo/internal/matrix"
)

// ── Request / response types ──────────────────────────────────────────────────

type notifyRequest struct {
	Title     string            `json:"title,omitempty"     example:"Deployment finished"`
	Message   string            `json:"message,omitempty"   example:"v2.3.0 deployed to prod"`
	Level     string            `json:"level,omitempty"     example:"info"`
	Animation string            `json:"animation,omitempty" example:"alert_pulse"`
	Duration  string            `json:"duration,omitempty"  example:"3s"`
	Priority  int               `json:"priority,omitempty"  example:"50"`
	Restore   string            `json:"restore,omitempty"   example:"background"`
	Params    map[string]string `json:"params,omitempty"`
}

type playRequest struct {
	Animation     string            `json:"animation"                  example:"alert_pulse"`
	Duration      string            `json:"duration,omitempty"         example:"2s"`
	Priority      int               `json:"priority,omitempty"         example:"50"`
	Restore       string            `json:"restore,omitempty"          example:"background"`
	InterruptMode string            `json:"interrupt_mode,omitempty"   example:"none"`
	Params        map[string]string `json:"params,omitempty"`
}

type brightnessRequest struct {
	Value uint8 `json:"value" example:"30"`
}

type colorRequest struct {
	R byte `json:"r" example:"0"`
	G byte `json:"g" example:"255"`
	B byte `json:"b" example:"85"`
}

type presetRequest struct {
	EffectID byte          `json:"effect_id"         example:"12"`
	Interval string        `json:"interval,omitempty" example:"90ms"`
	Color    *colorRequest `json:"color,omitempty"`
	R        byte          `json:"r,omitempty"`
	G        byte          `json:"g,omitempty"`
	B        byte          `json:"b,omitempty"`
}

type backgroundConfigRequest struct {
	Animation     string `json:"animation"      example:"matrix_rain_background"`
	RestoreOnIdle bool   `json:"restore_on_idle" example:"true"`
}

type eventAccepted struct {
	EventID string `json:"event_id"`
}

type requestAccepted struct {
	RequestID string `json:"request_id"`
}

type statusOK struct {
	Status string `json:"status" example:"ok"`
}

type statusOKPreset struct {
	Status    string `json:"status"    example:"ok"`
	Animation string `json:"animation" example:"matrix_rain_background"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type animationListResponse struct {
	Animations []string `json:"animations"`
}

type deviceListResponse struct {
	Devices []string `json:"devices"`
}

type queueResponse struct {
	Depth int    `json:"depth"`
	State string `json:"state"`
	Items []any  `json:"items"`
}

type queueClearResponse struct {
	Cleared int `json:"cleared"`
}

// ── Event endpoints ───────────────────────────────────────────────────────────

// @Summary		Publish a generic event
// @Description	Publishes a normalized event for async rule processing. Known override attributes (animation, restore, duration, interrupt_mode) are validated synchronously before publishing.
// @Tags		events
// @Accept		json
// @Produce		json
// @Param		device	path	string			true	"Device ID"
// @Param		body	body	events.Event	true	"Event payload"
// @Success		202		{object}	eventAccepted
// @Failure		400		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Failure		503		{object}	errorResponse	"Event bus backpressure timeout"
// @Router		/api/v1/devices/{device}/events [post]
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	_, deviceID := s.deviceFromRequest(r)

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
	event.Target = deviceID
	if err := s.bus.Publish(r.Context(), event); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, eventAccepted{EventID: event.ID})
}

// @Summary		Send a notification
// @Description	Publishes an event with source=http and type=notify. Rules matching that source/type decide which animation plays.
// @Tags		events
// @Accept		json
// @Produce		json
// @Param		device	path	string			true	"Device ID"
// @Param		body	body	notifyRequest	true	"Notification"
// @Success		202		{object}	eventAccepted
// @Failure		400		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Failure		503		{object}	errorResponse	"Event bus backpressure timeout"
// @Router		/api/v1/devices/{device}/notify [post]
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	_, deviceID := s.deviceFromRequest(r)

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
		Target:     deviceID,
		Text:       req.Message,
		Priority:   req.Priority,
		ReceivedAt: time.Now().UTC(),
		Attributes: attrs,
	}
	if err := s.bus.Publish(r.Context(), event); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, eventAccepted{EventID: event.ID})
}

// ── Animation / playback endpoints ────────────────────────────────────────────

// @Summary		Enqueue a renderable animation
// @Description	Directly enqueues a generated/frame animation on the device scheduler. Does not accept firmware preset IDs; use /preset/{animation} for those.
// @Tags		device
// @Accept		json
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string		true	"Device ID"
// @Param		body	body	playRequest	true	"Play request"
// @Success		202		{object}	requestAccepted
// @Failure		400		{object}	errorResponse	"Unknown, non-playable animation, or invalid parameters"
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Router		/api/v1/devices/{device}/play [post]
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)

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
	interruptMode := animations.InterruptMode(req.InterruptMode)
	if interruptMode == "" {
		interruptMode = animations.InterruptNone
	}
	if !validInterruptMode(interruptMode) {
		writeError(w, http.StatusBadRequest, "invalid interrupt_mode")
		return
	}

	request := animations.AnimationRequest{
		ID:            s.nextID("play"),
		AnimationID:   req.Animation,
		Params:        animations.Params(req.Params),
		Priority:      req.Priority,
		MaxDuration:   duration,
		InterruptMode: interruptMode,
		RestorePolicy: restore,
		CreatedAt:     time.Now().UTC(),
	}
	if err := scheduler.EnqueueRequest(r.Context(), request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, requestAccepted{RequestID: request.ID})
}

// @Summary		Play a firmware preset by animation ID
// @Description	Looks up a registered firmware_preset animation and sends its effect_id, interval, and color to the device. The idle background is restored afterward if restore_on_idle is enabled. Use GET /api/v1/animations/catalog to discover preset IDs.
// @Tags		device
// @Produce		json
// @Security	BearerAuth
// @Param		device		path	string	true	"Device ID"
// @Param		animation	path	string	true	"Firmware preset animation ID (e.g. matrix_rain_background)"
// @Success		200			{object}	statusOKPreset
// @Failure		400			{object}	errorResponse	"Animation exists but is not a firmware preset"
// @Failure		401			{object}	errorResponse
// @Failure		403			{object}	errorResponse
// @Failure		404			{object}	errorResponse	"Unknown animation ID or device"
// @Failure		502			{object}	errorResponse	"Matrix firmware error"
// @Failure		503			{object}	errorResponse	"Scheduler stopped or control canceled"
// @Router		/api/v1/devices/{device}/preset/{animation} [post]
func (s *Server) handlePlayPreset(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	animationID := chi.URLParam(r, "animation")

	preset, ok := s.registry.FirmwarePreset(animationID)
	if !ok {
		if entry, exists := s.registry.Entry(animationID); exists {
			_ = entry
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("animation %q is not a firmware preset; use /play for renderable animations", animationID))
			return
		}
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown animation %q", animationID))
		return
	}

	if err := scheduler.SetPreset(r.Context(), preset.EffectID, preset.Interval, preset.Color); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statusOKPreset{Status: "ok", Animation: animationID})
}

// ── Queue endpoints ───────────────────────────────────────────────────────────

// @Summary		Inspect the play queue
// @Description	Returns current queue depth, scheduler state, and snapshot of queued items.
// @Tags		device
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string	true	"Device ID"
// @Success		200		{object}	queueResponse
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Router		/api/v1/devices/{device}/queue [get]
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"depth": scheduler.QueueLen(),
		"state": scheduler.State(),
		"items": scheduler.QueueSnapshot(),
	})
}

// @Summary		Clear the play queue
// @Description	Removes all pending items from the queue. Returns the number of cleared items.
// @Tags		device
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string	true	"Device ID"
// @Success		200		{object}	queueClearResponse
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Router		/api/v1/devices/{device}/queue [delete]
func (s *Server) handleQueueClear(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	writeJSON(w, http.StatusOK, queueClearResponse{Cleared: scheduler.ClearQueue()})
}

// ── Animation discovery ───────────────────────────────────────────────────────

// @Summary		List playable animation IDs
// @Description	Returns only generated/frame animations that can be submitted to /play or /notify. Firmware preset IDs are excluded.
// @Tags		animations
// @Produce		json
// @Success		200	{object}	animationListResponse
// @Router		/api/v1/animations [get]
func (s *Server) handleAnimations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, animationListResponse{Animations: s.registry.RenderableIDs()})
}

// @Summary		Full animation catalog
// @Description	Returns all registered animations including firmware presets. Each entry exposes id, kind, and playable. Firmware presets have playable=false and may include effect_id, interval, and color.
// @Tags		animations
// @Produce		json
// @Success		200	{object}	animationCatalogResponse
// @Router		/api/v1/animations/catalog [get]
func (s *Server) handleAnimationCatalog(w http.ResponseWriter, _ *http.Request) {
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

// ── Background config ─────────────────────────────────────────────────────────

// @Summary		Get idle background configuration
// @Description	Returns the current desired idle animation for this device. When restore_on_idle is true, the scheduler converges back to this animation whenever the play queue empties.
// @Tags		device
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string	true	"Device ID"
// @Success		200		{object}	backgroundConfigRequest
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Router		/api/v1/devices/{device}/background [get]
func (s *Server) handleGetBackground(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	bg := scheduler.Background()
	writeJSON(w, http.StatusOK, backgroundConfigRequest{
		Animation:     bg.AnimationID,
		RestoreOnIdle: bg.AnimationID != "",
	})
}

// @Summary		Set idle background animation at runtime
// @Description	Replaces the desired idle animation without restarting the service. The scheduler converges to the new animation at the next idle opportunity. Set restore_on_idle=false or omit animation to disable idle convergence.
// @Tags		device
// @Accept		json
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string					true	"Device ID"
// @Param		body	body	backgroundConfigRequest	true	"Background config"
// @Success		200		{object}	statusOKPreset
// @Failure		400		{object}	errorResponse	"Unknown animation ID or validation error"
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Router		/api/v1/devices/{device}/background [put]
func (s *Server) handleSetBackground(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)

	var req backgroundConfigRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Animation != "" {
		entry, ok := s.registry.Entry(req.Animation)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown animation %q", req.Animation))
			return
		}
		_ = entry
	}

	cfg := matrix.BackgroundConfig{}
	if req.Animation != "" && req.RestoreOnIdle {
		cfg.AnimationID = req.Animation
	}
	if err := scheduler.SetBackground(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, statusOKPreset{Status: "ok", Animation: cfg.AnimationID})
}

// ── Device list ───────────────────────────────────────────────────────────────

// @Summary		List configured device IDs
// @Description	Returns the sorted list of device IDs as defined in the config file.
// @Tags		device
// @Produce		json
// @Security	BearerAuth
// @Success		200	{object}	deviceListResponse
// @Failure		401	{object}	errorResponse
// @Failure		403	{object}	errorResponse
// @Router		/api/v1/devices [get]
func (s *Server) handleDeviceList(w http.ResponseWriter, _ *http.Request) {
	ids := make([]string, 0, len(s.schedulers))
	for id := range s.schedulers {
		ids = append(ids, id)
	}
	writeJSON(w, http.StatusOK, deviceListResponse{Devices: ids})
}

// ── Matrix direct controls ────────────────────────────────────────────────────

// @Summary		Clear the display
// @Description	Sets all pixels to black. If restore_on_idle is enabled, the idle background will be reapplied afterward.
// @Tags		device
// @Accept		json
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string	true	"Device ID"
// @Success		200		{object}	statusOK
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Failure		502		{object}	errorResponse	"Matrix firmware error"
// @Failure		503		{object}	errorResponse
// @Router		/api/v1/devices/{device}/matrix/clear [post]
func (s *Server) handleMatrixClear(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	if err := scheduler.Clear(r.Context()); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statusOK{Status: "ok"})
}

// @Summary		Set display brightness
// @Description	Sets global brightness (0–255). Changes take effect immediately.
// @Tags		device
// @Accept		json
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string				true	"Device ID"
// @Param		body	body	brightnessRequest	true	"Brightness value"
// @Success		200		{object}	statusOK
// @Failure		400		{object}	errorResponse
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Failure		502		{object}	errorResponse	"Matrix firmware error"
// @Router		/api/v1/devices/{device}/matrix/brightness [post]
func (s *Server) handleMatrixBrightness(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	var req brightnessRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := scheduler.SetBrightness(r.Context(), req.Value); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statusOK{Status: "ok"})
}

// @Summary		Apply a firmware effect preset
// @Description	Starts a built-in firmware animation effect with custom interval and color. Use effect_id=0 to stop the current effect.
// @Tags		device
// @Accept		json
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string			true	"Device ID"
// @Param		body	body	presetRequest	true	"Preset parameters"
// @Success		200		{object}	statusOK
// @Failure		400		{object}	errorResponse	"Invalid interval or effect_id"
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Failure		502		{object}	errorResponse	"Matrix firmware error"
// @Router		/api/v1/devices/{device}/matrix/preset [post]
func (s *Server) handleMatrixPreset(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
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
	if err := scheduler.SetPreset(r.Context(), req.EffectID, interval, color); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statusOK{Status: "ok"})
}

// @Summary		Fill display with a solid colour
// @Description	Sets every pixel to a single RGB color immediately.
// @Tags		device
// @Accept		json
// @Produce		json
// @Security	BearerAuth
// @Param		device	path	string			true	"Device ID"
// @Param		body	body	colorRequest	true	"RGB color"
// @Success		200		{object}	statusOK
// @Failure		400		{object}	errorResponse
// @Failure		401		{object}	errorResponse
// @Failure		403		{object}	errorResponse
// @Failure		404		{object}	errorResponse	"Unknown device"
// @Failure		502		{object}	errorResponse	"Matrix firmware error"
// @Router		/api/v1/devices/{device}/matrix/fill [post]
func (s *Server) handleMatrixFill(w http.ResponseWriter, r *http.Request) {
	scheduler, _ := s.deviceFromRequest(r)
	var req colorRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := scheduler.Fill(r.Context(), matrix.RGB{R: req.R, G: req.G, B: req.B}); err != nil {
		writeMatrixControlError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, statusOK{Status: "ok"})
}

// ── Catalog types ─────────────────────────────────────────────────────────────

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

// ── Helpers ───────────────────────────────────────────────────────────────────

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
	if interruptMode := attrs["interrupt_mode"]; interruptMode != "" && !validInterruptMode(animations.InterruptMode(interruptMode)) {
		return errors.New("invalid interrupt_mode")
	}
	return nil
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

func validInterruptMode(mode animations.InterruptMode) bool {
	switch mode {
	case animations.InterruptNone, animations.InterruptHigherPriority, animations.InterruptCritical:
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
	writeJSON(w, status, errorResponse{Error: message})
}
