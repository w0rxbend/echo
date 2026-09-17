package app

import (
	"testing"

	"github.com/worxbend/echo/internal/matrix"
)

// publicKindMap in internal/animations is keyed by strings that internal/matrix
// owns, and it cannot reference the matrix constants: matrix imports animations,
// so the arrow only points one way. That leaves the two lists joined by nothing
// but a hard-coded literal, and every consumer of a failed projection drops
// quietly -- /readyz blanks background.kind, and the restore, dirty, converged,
// state and next-retry metrics stop being emitted for the whole device. This is
// the join the compiler cannot make.
func TestEveryBackgroundKindProjectsToAPublicKind(t *testing.T) {
	for _, kind := range matrix.BackgroundKinds() {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			publicKind, ok := publicBackgroundKind(kind)
			if !ok {
				t.Fatalf("publicBackgroundKind(%q) failed; animations.publicKindMap has no entry for it, so /readyz and every background metric would silently drop this kind", kind)
			}
			if publicKind == "" {
				t.Fatalf("publicBackgroundKind(%q) projected to the empty public kind", kind)
			}
			if string(publicKind) == string(kind) && kind == matrix.BackgroundKindRenderable {
				t.Fatalf("publicBackgroundKind(%q) leaked the internal spelling into the public vocabulary", kind)
			}

			label, ok := publicBackgroundKindLabel(kind)
			if !ok {
				t.Fatalf("publicBackgroundKindLabel(%q) failed after publicBackgroundKind succeeded", kind)
			}
			if label != string(publicKind) {
				t.Fatalf("publicBackgroundKindLabel(%q) = %q, want %q", kind, label, publicKind)
			}
		})
	}
}

// The empty kind means no background is configured. It must fail projection so
// the callers skip the field and the metrics entirely, rather than emitting an
// empty kind label.
func TestEmptyBackgroundKindDoesNotProject(t *testing.T) {
	if got, ok := publicBackgroundKind(""); ok {
		t.Fatalf("publicBackgroundKind(\"\") = %q, true; want a failed projection", got)
	}
	if got, ok := publicBackgroundKindLabel(""); ok {
		t.Fatalf("publicBackgroundKindLabel(\"\") = %q, true; want a failed projection", got)
	}
}

func TestUnknownBackgroundKindDoesNotProject(t *testing.T) {
	if got, ok := publicBackgroundKind(matrix.BackgroundKind("not_a_kind")); ok {
		t.Fatalf("publicBackgroundKind(unknown) = %q, true; want a failed projection", got)
	}
}
