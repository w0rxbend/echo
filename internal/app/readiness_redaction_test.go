package app

import (
	"testing"

	"github.com/worxbend/echo/internal/integrations/httpapi"
	"github.com/worxbend/echo/internal/matrix"
)

// The restore path stores a raw err.Error(), so a dial failure names the
// panel's LAN address and port. /readyz is mounted outside the admin gate,
// which is fine for a probe but not for that string.
const dialFailure = "dial tcp 10.0.3.44:4210: connect: connection refused"

func TestDeviceReadinessEntryRedactsLastErrorOnUntrustedOpsPlane(t *testing.T) {
	health := matrix.Health{
		BackgroundLastRestoreError:      dialFailure,
		BackgroundLastRestoreErrorClass: matrix.ErrorKindRetryable,
	}

	tests := []struct {
		name          string
		redact        bool
		wantLastError string
	}{
		{name: "trusted bind keeps the detail", redact: false, wantLastError: dialFailure},
		{name: "untrusted bind drops the detail", redact: true, wantLastError: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := deviceReadinessEntry(health, matrix.BackgroundConvergenceProjection{}, "", tt.redact)

			if entry.Background.LastError != tt.wantLastError {
				t.Fatalf("background.last_error = %q, want %q", entry.Background.LastError, tt.wantLastError)
			}
			// The class is what a probe or an alert reads, and it names no host,
			// so it survives either way. Redaction that took it too would make
			// "retryable" and "permanent" indistinguishable off loopback.
			if entry.Background.LastErrorClass != matrix.ErrorKindRetryable {
				t.Fatalf("background.last_error_class = %q, want %q", entry.Background.LastErrorClass, matrix.ErrorKindRetryable)
			}
		})
	}
}

func TestOpsPlaneIsUntrustedFollowsTheAdminGate(t *testing.T) {
	const tokenEnv = "ECHO_TEST_READYZ_ADMIN_TOKEN"

	newServer := func(t *testing.T, addr string) *httpapi.Server {
		t.Helper()
		server, err := httpapi.New(httpapi.Options{ServerAddr: addr, AdminTokenEnv: tokenEnv})
		if err != nil {
			t.Fatalf("httpapi.New(%q): %v", addr, err)
		}
		return server
	}

	t.Setenv(tokenEnv, "s3cret")

	tests := []struct {
		name    string
		httpAPI func(t *testing.T) *httpapi.Server
		want    bool
	}{
		{
			// Every readiness test in this package builds an App without an
			// httpAPI, so the nil case has to keep the full detail.
			name:    "no http server",
			httpAPI: func(*testing.T) *httpapi.Server { return nil },
			want:    false,
		},
		{
			name:    "loopback bind",
			httpAPI: func(t *testing.T) *httpapi.Server { return newServer(t, "127.0.0.1:8080") },
			want:    false,
		},
		{
			name:    "wildcard bind",
			httpAPI: func(t *testing.T) *httpapi.Server { return newServer(t, "0.0.0.0:8080") },
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &App{httpAPI: tt.httpAPI(t)}
			if got := a.opsPlaneIsUntrusted(); got != tt.want {
				t.Fatalf("opsPlaneIsUntrusted() = %v, want %v", got, tt.want)
			}
		})
	}
}
