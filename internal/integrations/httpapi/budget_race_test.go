//go:build race

package httpapi_test

// The race detector makes every one of these tests slower: they wait on real
// goroutines doing real loopback I/O, and instrumented builds run five to ten
// times slower than an ordinary one. A budget tuned on an idle workstation is a
// flake on a busy two-core CI runner, so raise them all here rather than
// guessing per call site.
const testBudgetScale = 5
