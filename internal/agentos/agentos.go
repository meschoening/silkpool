// Package agentos is the agent's OS on Linux, macOS and Windows: it
// carries out the node's effects on the machine, as agent.OS asks,
// through package slot for the slots, the service account they run as
// and their runner units (systemd's on Linux, launchd's jobs on macOS,
// the slots' services and their slot hosts on Windows), and package
// runnerinstall for the runner application, and it follows each runner's
// listener log. The wiring is from "Run Linux jobs end to end", "Run
// macOS jobs end to end on the Mac" and "Run Windows jobs end to end on
// the Windows VM", and the listener's lines from "Define Silkpool's
// events, audit trail, notices and redaction".
//
// Each slot's effects run one at a time, in the order the node asked for
// them, and the installs' one at a time too. A runner's listener is
// followed on a goroutine of its own, from its start, or from the
// agent's start for one still alive, until its unit ends. From its job's
// start, the OS also looks for the job's worker in the unit, its cgroup
// on Linux, the processes descended from its launchd job's on macOS and
// its slot host's job object on Windows, until it's up, as "Decide what
// Emergency stop does in a job's first second" has it. On macOS and
// Windows it also works out the shared places in the background, as it
// starts and daily, which the exposure check needs. On Windows each
// slot's preparation reads Smart App Control's state first.
package agentos
