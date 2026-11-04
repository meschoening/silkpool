// Package slot runs a node's slots on its OS: their slot accounts, the
// units their runners run in, their exposure check and their reset (ADRs
// 0005 to 0007).
//
// So far it runs Linux slots. Slots prepares their accounts, checking
// them for drift, and starts, stops, waits for and finds again their
// runners' units through systemd. Its Ready, Reset and DeleteAccount
// carry out the node's PrepareSlot, ResetSlot and DeleteAccount, and
// return the events the node applies. Helper runs silkpool as the
// helpers it starts as a slot account: the launcher each runner unit
// runs, the reset's kill helper and the exposure check. Layout says
// where the slots' folders and runners' logs are.
package slot
