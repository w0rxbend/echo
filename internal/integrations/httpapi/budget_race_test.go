//go:build race

package httpapi_test

// The race detector makes every one of these tests slower: they wait on real
// goroutines doing real loopback I/O, and instrumented builds run five to ten
// times slower than an ordinary one. A budget tuned on an idle workstation is a
// flake on a busy two-core CI runner, so raise them all here rather than
// guessing per call site.
//
// Do not try to justify the number by reproducing the failure on a workstation.
// 220 runs across every configuration this one can construct -- two-core
// pinning, the scale neutralised back to 1, CPU hogs pinned to the same two
// cores -- produced zero failures. Pinning bounds core count, not contention,
// and these tests are about 8% CPU, so the scheduler favours them over the
// hogs: under two spinners a run took 1.180s against 1.172s idle. The
// conditions that produced the original 1-in-8 belong to the runner, not to
// the core count. Keeping the scale costs nothing when tests pass, because
// budgets bound failures rather than successes.
const testBudgetScale = 5
