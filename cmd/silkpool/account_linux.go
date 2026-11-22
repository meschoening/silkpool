package main

import (
	"context"

	"github.com/meschoening/silkpool/internal/slot"
)

func init() {
	modes["make-service-account"] = makeServiceAccount
	modes["delete-service-account"] = deleteServiceAccount
}

// makeServiceAccount makes the machine's service account, which every
// slot runs as, or checks it, restoring what Silkpool owns, as the node
// package's postinstall script asks (ADR 0007). It fails, saying why, if
// an account with its name isn't Silkpool's or was added to another
// group.
func makeServiceAccount(ctx context.Context, args []string) int {
	return accountMode("make-service-account", args, slot.Installed.PrepareAccount)
}

// deleteServiceAccount deletes the machine's service account, as the
// node package's preremove script asks once the machine isn't enrolled
// and the agent has stopped (ADR 0007). It fails, saying why, and
// deletes nothing, while a process holds the account's UID, or if the
// account isn't Silkpool's.
func deleteServiceAccount(ctx context.Context, args []string) int {
	return accountMode("delete-service-account", args, slot.Installed.DeleteAccount)
}
