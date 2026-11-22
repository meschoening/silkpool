package slot

import (
	"context"
	"errors"
	"time"

	"github.com/meschoening/silkpool/internal/availability"
	"github.com/meschoening/silkpool/internal/node"
)

// SurveyEvery is how often the shared places are worked out again: the
// agent surveys daily (#19), and a slot isn't readied from a survey
// older than that.
const SurveyEvery = 24 * time.Hour

// Ready readies slot n for a runner, as PrepareSlot asks, and returns
// what the node applies: Prepared, or PrepareFailed with SlotCantStart or
// SlotExposed, as on Linux. It runs Prepare's steps, with a survey
// before the clean: it checks the service account and makes the slot's
// folders; unless a survey of the account no older than SurveyEvery is
// saved, it surveys the shared places, as after the Mac enrolls; it runs
// the account-wide clean if it's due, whose sweep needs that survey;
// and then the machine's exposure check (Check). Any failure but a hit
// is SlotCantStart: the slot can't be vouched for.
func (s *Slots) Ready(ctx context.Context, n int) node.Event {
	err := s.prepare(ctx, n)
	var a ServiceAccount
	if err == nil {
		a, err = s.Account(ctx)
	}
	if err == nil {
		err = s.surveyed(ctx, a)
	}
	if err == nil {
		err = s.cleanFirst(ctx)
	}
	if err == nil {
		err = s.Check(ctx, a)
	}
	var exposed *Exposed
	var cant *CantStart
	switch {
	case err == nil:
		return node.Prepared{Slot: n}
	case errors.As(err, &exposed):
		return node.PrepareFailed{Slot: n, Reason: node.SlotExposed, Detail: exposed.Detail}
	case errors.As(err, &cant):
		return node.PrepareFailed{Slot: n, Reason: node.SlotCantStart, Detail: cant.Detail}
	}
	return node.PrepareFailed{Slot: n, Reason: node.SlotCantStart, Detail: detail(err)}
}

// surveyed surveys the shared places as a unless the latest survey, of
// a, succeeded less than SurveyEvery ago. Slots readied together, and the
// agent's own survey, take turns, so that one survey serves them all.
func (s *Slots) surveyed(ctx context.Context, a ServiceAccount) error {
	s.surveying.Lock()
	defer s.surveying.Unlock()
	if sv, err := s.readSurvey(a); err == nil {
		if age := s.now().Sub(sv.Time); age >= 0 && age < SurveyEvery {
			return nil
		}
	}
	return s.Survey(ctx, a)
}

// Resurvey works out the shared places again, as the service account,
// whatever the latest survey: the agent runs it as it starts and every
// SurveyEvery after, in the background. It takes turns with the surveys
// Ready runs.
func (s *Slots) Resurvey(ctx context.Context) error {
	a, err := s.Account(ctx)
	if err != nil {
		return err
	}
	s.surveying.Lock()
	defer s.surveying.Unlock()
	return s.Survey(ctx, a)
}

// Owner returns the service account's UID and GID, once its record
// proves it's the one Silkpool made (Account), which a runner root's
// clones of its install are given.
func (s *Slots) Owner(ctx context.Context) (uid, gid int, err error) { return s.ids(ctx) }

// SetBudget refuses every budget: launchd can't hold a Mac's slots to
// one, and the node never asks for one on a Mac (node.ErrNoBudget).
func (s *Slots) SetBudget(context.Context, availability.Budget) error { return node.ErrNoBudget }
