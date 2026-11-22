package main

import (
	"context"

	"github.com/meschoening/silkpool/internal/slot"
)

func init() {
	modes["make-service-account"] = makeServiceAccount
}

// makeServiceAccount makes the Mac's service account, which every slot
// runs as, or checks it, restoring what Silkpool owns, as the .pkg's
// postinstall script asks before it starts the agent (ADR 0007). It
// fails, saying why, if an account with its name isn't Silkpool's or was
// added to another group, and the postinstall then fails the install.
// sudo silkpool uninstall deletes the account.
func makeServiceAccount(ctx context.Context, args []string) int {
	return accountMode("make-service-account", args, func() error {
		s, err := slot.Open(ctx, slot.Installed)
		if err != nil {
			return err
		}
		_, err = s.MakeAccount(ctx)
		return err
	})
}
