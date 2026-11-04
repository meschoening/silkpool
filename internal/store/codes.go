package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
)

// The stored form names every enum value and every refusal by a stable
// code of its own, never by the core's numbering or wording, so a core
// can reorder its constants or reword its errors without a migration.

// Codes maps each value of an enum to its stable code. A zero value
// whose code is "" is left out of the stored form.
type Codes[T comparable] struct {
	name   string
	codes  map[T]string
	values map[string]T
}

// NewCodes returns the codes of an enum called name.
func NewCodes[T comparable](name string, codes map[T]string) Codes[T] {
	values := make(map[string]T, len(codes))
	for v, c := range codes {
		if _, dup := values[c]; dup {
			panic(fmt.Sprintf("store: two %s values have the code %q", name, c))
		}
		values[c] = v
	}
	return Codes[T]{name: name, codes: codes, values: values}
}

// Code returns v's code, or an error if v has none.
func (c Codes[T]) Code(v T) (string, error) {
	code, ok := c.codes[v]
	if !ok {
		return "", fmt.Errorf("%s %v has no stored code", c.name, v)
	}
	return code, nil
}

// Value returns the value whose code is code, or an error if there's
// none.
func (c Codes[T]) Value(code string) (T, error) {
	v, ok := c.values[code]
	if !ok {
		return v, fmt.Errorf("%q isn't a stored %s", code, c.name)
	}
	return v, nil
}

// Has reports whether v has a code.
func (c Codes[T]) Has(v T) bool {
	_, ok := c.codes[v]
	return ok
}

var (
	intents = NewCodes("intent", map[availability.Intent]string{
		availability.PausedIndefinitely: "paused-indefinitely",
		availability.Paused:             "paused",
		availability.OnDuty:             "on-duty",
	})
	phases = NewCodes("slot phase", map[node.Phase]string{
		node.Free:            "free",
		node.Preparing:       "preparing",
		node.Requesting:      "requesting",
		node.Hosting:         "hosting",
		node.Resetting:       "resetting",
		node.RetryingReset:   "retrying-reset",
		node.DeletingAccount: "deleting-account",
		node.RetryingDelete:  "retrying-delete",
		node.Deleted:         "deleted",
	})
	retirements = NewCodes("retirement", map[node.Retirement]string{
		node.NotRetiring:     "",
		node.Retiring:        "retiring",
		node.ClosingAfterJob: "closing-after-job",
		node.Stopping:        "stopping",
		node.Cancelling:      "cancelling",
		node.StoppingLost:    "stopping-lost",
	})
	reasons = NewCodes("reason", map[node.Reason]string{
		0:                         "",
		node.HubUnreachable:       "hub-unreachable",
		node.GitHubRefused:        "github-refused",
		node.GitHubUnavailable:    "github-unavailable",
		node.RunnerOffline:        "runner-offline",
		node.RunnerAppUnavailable: "runner-app-unavailable",
		node.SlotCantStart:        "slot-cant-start",
		node.SlotExposed:          "slot-exposed",
		node.VersionMismatch:      "version-mismatch",
		node.HubIdentityChanged:   "hub-identity-changed",
		node.WrongNodeKey:         "wrong-node-key",
	})
	surfaces = NewCodes("surface", map[node.Surface]string{
		0:                   "",
		node.Dashboard:      "dashboard",
		node.CommandLine:    "command-line",
		node.Tray:           "tray",
		node.HubCommandLine: "hub-command-line",
		node.AutoResume:     "auto-resume",
		node.Hub:            "hub",
	})
	outcomes = NewCodes("job outcome", map[node.Outcome]string{
		0:                   "",
		node.JobFinished:    "finished",
		node.JobInterrupted: "interrupted",
		node.JobCancelled:   "cancelled",
	})
	endings = NewCodes("Degraded ending", map[node.Ending]string{
		0:                   "",
		node.Recovered:      "recovered",
		node.EndedByPause:   "paused",
		node.EndedByRemoval: "removed",
	})
	noticeKinds = NewCodes("notice kind", map[node.NoticeKind]string{
		node.NoticeInterruptedJob: "interrupted-job",
		node.NoticeUpdateFailed:   "update-failed",
		node.NoticeSlotExposed:    "slot-exposed",
	})
)

// days names the days of the week, Sunday first, as time.Weekday counts
// them.
var days = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// refusals are the errors a reply can be refused with that have a stable
// code. An error matches the first one it Is.
var refusals = []struct {
	code string
	err  error
}{
	{"stale", availability.ErrStale},
	{"auto-resume-off", availability.ErrAutoResumeOff},
	{"invalid", availability.ErrInvalid},
	{"no-id", node.ErrNoID},
	{"no-target", node.ErrNoTarget},
	{"removing", node.ErrRemoving},
	{"hub-unreachable", node.ErrHubUnreachable},
	{"not-enrolled", node.ErrNotEnrolled},
	{"enrolled", node.ErrEnrolled},
	{"hub-identity-changed", node.ErrHubIdentityChanged},
	{"condition", node.ErrCondition},
	{"no-budget", node.ErrNoBudget},
}

// An Error is an error's stored form: the stable code of the refusal it
// is, or "" if it's none of them, and its message.
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// FromError returns err's stored form, or nil for nil.
func FromError(err error) *Error {
	if err == nil {
		return nil
	}
	e := &Error{Message: err.Error()}
	for _, r := range refusals {
		if errors.Is(err, r.err) {
			e.Code = r.code
			break
		}
	}
	return e
}

// ToError returns the error e stores, or nil for nil. One with a code is
// that refusal itself when its message is the refusal's own, and wraps it
// otherwise, so errors.Is finds it either way.
func (e *Error) ToError() error {
	if e == nil {
		return nil
	}
	for _, r := range refusals {
		if r.code != e.Code || e.Code == "" {
			continue
		}
		if e.Message == r.err.Error() {
			return r.err
		}
		return storedError{refusal: r.err, message: e.Message}
	}
	return errors.New(e.Message)
}

// storedError is a refusal with a message of its own, such as
// availability.ErrInvalid with its detail.
type storedError struct {
	refusal error
	message string
}

func (e storedError) Error() string { return e.message }
func (e storedError) Unwrap() error { return e.refusal }

// Stamp returns t's stored form: UTC, with no monotonic reading, or ""
// for the zero time.
func Stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Unstamp returns the time s stores, in UTC.
func Unstamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q isn't a stored time", s)
	}
	return t.UTC(), nil
}
