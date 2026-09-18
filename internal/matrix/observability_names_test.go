package matrix

import (
	"testing"

	"github.com/worxbend/echo/internal/observability/obstest"
)

// assertSchedulerCallbackNamesAreListed catches an unenumerated name only for
// the names a test actually drives, and reconnect_delay, probe_failure and
// reconnect_failure are reachable only through waitReady and the reconnect
// loop. This reads the package source instead, so those sites are covered too.
func TestObservabilityCallbackSitesAreAllListed(t *testing.T) {
	listed := append(SchedulerObservabilityCallbackNames(), TCPClientObservabilityCallbackNames()...)
	obstest.AssertCallbackSitesAreListed(t, ".",
		"SchedulerObservabilityCallbackNames or TCPClientObservabilityCallbackNames", listed)
}

func TestTCPClientObservabilityCallbackNamesAreDistinct(t *testing.T) {
	obstest.AssertNamesAreDistinct(t, "TCPClientObservabilityCallbackNames", TCPClientObservabilityCallbackNames())
}
