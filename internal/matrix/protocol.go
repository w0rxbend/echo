package matrix

import (
	"fmt"
)

const (
	magic0          byte = 0x4C
	magic1          byte = 0x4D
	protocolVersion byte = 0x01
	responseCommand byte = 0x80

	maxPayloadSize = 255
	headerSize     = 5
	checksumSize   = 1
	responseSize   = 6
)

type command byte

const (
	commandPing command = iota
	commandClear
	commandSetBrightness
	commandFill
	commandSetPixel
	commandSetFrame
	commandSetPanelEnabled
	commandSetStaticColor
	commandSetPresetEffect
	commandUploadCustomFrame
	commandStopEffect
)

var commandStrings = map[command]string{
	commandPing:            "ping",
	commandClear:           "clear",
	commandSetBrightness:   "set_brightness",
	commandFill:            "fill",
	commandSetPixel:        "set_pixel",
	commandSetFrame:        "set_frame",
	commandSetPanelEnabled: "set_panel_enabled",
	commandSetStaticColor:  "set_static_color",
	commandSetPresetEffect: "set_preset_effect",
	commandUploadCustomFrame:"upload_custom_frame",
	commandStopEffect:      "stop_effect",
}

func (c command) String() string {
	if value, ok := commandStrings[c]; ok {
		return value
	}
	return fmt.Sprintf("unknown_0x%02x", byte(c))
}

type Status byte

const (
	StatusOK Status = iota
	StatusBadMagic
	StatusUnsupportedVersion
	StatusUnknownCommand
	StatusInvalidLength
	StatusChecksumMismatch
)

func checksum(data []byte) byte {
	var value byte
	for _, b := range data {
		value ^= b
	}
	return value
}

func buildCommandFrame(cmd command, payload []byte) ([]byte, error) {
	if len(payload) > maxPayloadSize {
		return nil, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), maxPayloadSize)
	}

	frame := make([]byte, 0, headerSize+len(payload)+checksumSize)
	frame = append(frame, magic0, magic1, protocolVersion, byte(cmd), byte(len(payload)))
	frame = append(frame, payload...)
	frame = append(frame, checksum(frame))
	return frame, nil
}

func parseResponse(response []byte) error {
	if len(response) != responseSize {
		return &ProtocolError{Reason: fmt.Sprintf("response length %d, want %d", len(response), responseSize), Response: response}
	}
	if response[0] != magic0 || response[1] != magic1 {
		return &ProtocolError{Reason: "bad response magic", Response: response}
	}
	if response[2] != protocolVersion {
		return &ProtocolError{Reason: fmt.Sprintf("bad response version 0x%02x", response[2]), Response: response}
	}
	if response[3] != responseCommand {
		return &ProtocolError{Reason: fmt.Sprintf("bad response command 0x%02x", response[3]), Response: response}
	}
	if got, want := response[5], checksum(response[:5]); got != want {
		return &ProtocolError{Reason: fmt.Sprintf("bad response checksum 0x%02x, want 0x%02x", got, want), Response: response}
	}

	return statusError(Status(response[4]))
}

type ProtocolError struct {
	Reason   string
	Response []byte
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%v: %s", ErrProtocol, e.Reason)
}

func (e *ProtocolError) Unwrap() error {
	return ErrProtocol
}

var statusErrors = map[Status]error{
	StatusBadMagic:           ErrStatusBadMagic,
	StatusUnsupportedVersion:  ErrStatusUnsupportedVersion,
	StatusUnknownCommand:      ErrStatusUnknownCommand,
	StatusInvalidLength:       ErrStatusInvalidLength,
	StatusChecksumMismatch:    ErrStatusChecksumMismatch,
}

var statusToString = map[Status]string{
	StatusOK:               "ok",
	StatusBadMagic:         "bad magic",
	StatusUnsupportedVersion: "unsupported version",
	StatusUnknownCommand:    "unknown command",
	StatusInvalidLength:     "invalid length",
	StatusChecksumMismatch:  "checksum mismatch",
}

var statusToLabel = map[Status]string{
	StatusOK:                "ok",
	StatusBadMagic:          "bad_magic",
	StatusUnsupportedVersion: "unsupported_version",
	StatusUnknownCommand:     "unknown_command",
	StatusInvalidLength:      "invalid_length",
	StatusChecksumMismatch:   "checksum_mismatch",
}

type StatusError struct {
	Status Status
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("matrix command failed: status 0x%02x (%s)", byte(e.Status), e.Status.String())
}

func (e *StatusError) Unwrap() error {
	if err, ok := statusErrors[e.Status]; ok {
		return err
	}
	return ErrStatusUnknown
}

func (s Status) String() string {
	if value, ok := statusToString[s]; ok {
		return value
	}
	return "unknown"
}

func (s Status) Label() string {
	if value, ok := statusToLabel[s]; ok {
		return value
	}
	return "unknown"
}

func statusError(status Status) error {
	if status == StatusOK {
		return nil
	}
	return &StatusError{Status: status}
}
