package app

import (
	"testing"
	"time"

	"github.com/worxbend/echo/internal/config"
)

// TestHealthMetricsInterval covers the selection of the health-metrics ticker
// period. The regression it guards is the third case: an earlier sentinel of
// time.Second could not tell "no interval chosen yet" apart from a device
// configured at exactly one second, so a later, larger interval replaced it.
func TestHealthMetricsInterval(t *testing.T) {
	tests := []struct {
		name      string
		intervals []time.Duration
		want      time.Duration
	}{
		{
			name:      "no devices falls back to one second",
			intervals: nil,
			want:      time.Second,
		},
		{
			name:      "unconfigured intervals fall back to one second",
			intervals: []time.Duration{0, 0},
			want:      time.Second,
		},
		{
			name:      "one second first is not overwritten by a larger later interval",
			intervals: []time.Duration{5 * time.Second, time.Second, 3 * time.Second},
			want:      time.Second,
		},
		{
			name:      "smallest wins regardless of position",
			intervals: []time.Duration{4 * time.Second, 250 * time.Millisecond, 2 * time.Second},
			want:      250 * time.Millisecond,
		},
		{
			name:      "a single device sets the interval",
			intervals: []time.Duration{7 * time.Second},
			want:      7 * time.Second,
		},
		{
			name:      "devices without an interval are skipped",
			intervals: []time.Duration{0, 6 * time.Second, 0},
			want:      6 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{cfg: config.Config{Devices: map[string]*config.DeviceConfig{}}}
			for i, interval := range tt.intervals {
				id := string(rune('a' + i))
				app.devices = append(app.devices, &appDevice{id: id})
				app.cfg.Devices[id] = &config.DeviceConfig{HeartbeatInterval: interval}
			}

			if got := app.healthMetricsInterval(); got != tt.want {
				t.Errorf("healthMetricsInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHealthMetricsIntervalSkipsMissingDeviceConfig guards the nil-config branch:
// a device present in the app but absent from the config map must not panic.
func TestHealthMetricsIntervalSkipsMissingDeviceConfig(t *testing.T) {
	app := &App{
		cfg:     config.Config{Devices: map[string]*config.DeviceConfig{}},
		devices: []*appDevice{{id: "ghost"}},
	}

	if got := app.healthMetricsInterval(); got != time.Second {
		t.Errorf("healthMetricsInterval() = %v, want %v", got, time.Second)
	}
}
