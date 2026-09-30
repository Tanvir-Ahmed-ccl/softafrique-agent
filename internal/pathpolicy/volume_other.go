//go:build !windows

package pathpolicy

// classifyVolume cannot read a drive type off Windows, and says so by returning
// false for decidable.
//
// The consequence is that RequireFixed applies only the network-share half of the
// rule here, and a path on a DVD drive passes on macOS or Linux. That is
// deliberate and it is the honest answer rather than a failing one: the
// alternative is a developer machine that refuses to install the agent, and a
// check that cannot be exercised is a check nobody keeps. The Windows build is
// the one that ships, CI cross-compiles and runs it on a real Windows runner, and
// only a run on Windows exercises the volume half.
//
// A test that wants the volume rule checked asserts it in a file built only for
// Windows, so it is skipped with a visible skip rather than passing quietly.
func classifyVolume(string) (Kind, bool) { return KindUnknown, false }
