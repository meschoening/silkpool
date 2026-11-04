package wording

import (
	"fmt"
	"time"
)

// The dashboard's words for what only the Hub sees, from "Prototype the
// Hub dashboard":
// a node's last-known report, and setup's checks.

// NotConnected says the dashboard shows a node's last report from
// before it lost the node at since.
func (c Clock) NotConnected(since time.Time) string {
	return fmt.Sprintf("Not connected since %s. Jobs may have finished since this report.", c.When(since))
}

// Started is when a job in a last-known report started, which a busy row
// shows in place of how long it's run: "started 2:37 PM".
func (c Clock) Started(at time.Time) string { return "started " + c.When(at) }

// ClockOff says how far the Hub's clock is ahead of GitHub's, skew, or
// behind it when skew is negative: "The clock is 4 min fast."
func ClockOff(skew time.Duration) string {
	way := "fast"
	if skew < 0 {
		way, skew = "slow", -skew
	}
	amount := fmt.Sprintf("%d s", int(skew.Round(time.Second)/time.Second))
	if skew >= time.Minute {
		amount = Duration(skew)
	}
	return fmt.Sprintf("The clock is %s %s.", amount, way)
}
