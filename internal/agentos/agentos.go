// Package agentos is the agent's OS on Linux: it carries out the node's
// effects on the machine, as agent.OS asks, through package slot for the
// slots, their accounts and runner units, and package runnerinstall for
// the runner application, and it follows each runner's listener log.
// The wiring is from "Run Linux jobs end to end", and the
// listener's lines from "Define Silkpool's events, audit trail, notices
// and redaction".
//
// Each slot's effects run one at a time, in the order the node asked for
// them, and the installs' one at a time too. A runner's listener is
// followed on a goroutine of its own, from its start, or from the
// agent's start for one still alive, until its unit ends. From its job's
// start, the OS also looks for the job's worker in the unit's cgroup
// until it's up, as "Decide what Emergency stop does in a job's first
// second"
// has it.
//
// So far it runs Linux nodes.
package agentos
