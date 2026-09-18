package events

import (
	"testing"

	"github.com/worxbend/echo/internal/observability/obstest"
)

// The bus's per-name panic counters are registered by iterating
// ObservabilityCallbackNames, and the test that scrapes them counts the emitted
// series against that same list -- so it cannot notice a name recorded at a
// Run/RecoverFrom site the list omits. This reads the package source, which is
// the only thing that can.
func TestObservabilityCallbackSitesAreAllListed(t *testing.T) {
	obstest.AssertCallbackSitesAreListed(t, ".", "ObservabilityCallbackNames", ObservabilityCallbackNames())
}

func TestObservabilityCallbackNamesAreDistinct(t *testing.T) {
	obstest.AssertNamesAreDistinct(t, "ObservabilityCallbackNames", ObservabilityCallbackNames())
}
