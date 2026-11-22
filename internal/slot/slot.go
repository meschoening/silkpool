// Package slot runs a node's slots on its OS: the machine's service
// account, which every slot runs as (#214), the units their runners run
// in, the exposure check and the reset (ADRs 0005 to 0007).
//
// On Linux, Slots checks the service account for drift, and starts,
// stops, waits for and finds again the runners' units through systemd.
// Its Ready, Reset and DeleteSlot carry out the node's PrepareSlot,
// ResetSlot and DeleteSlot, and return the events the node applies. The
// reset is the slot's own, and the account-wide clean runs once every
// slot is idle (Clean). The package makes the account and deletes it
// (Layout.PrepareAccount and Layout.DeleteAccount). Helper runs silkpool
// as the helpers it starts as the service account: the launcher each
// runner unit runs, the clean's kill helper and the exposure check.
// Layout says where the account's home, the slots' folders and the
// runners' logs are.
//
// On macOS, every slot runs as the machine's one service account, a role
// account (#214). Slots makes it, as install does (MakeAccount), and
// checks it for drift as the agent starts and as it prepares each slot
// (CheckAccount and Prepare, internal/roleaccount). It starts, stops,
// waits for and finds again the runners' launchd jobs (internal/launchd).
// Its Reset and DeleteSlot carry out the node's ResetSlot and
// DeleteSlot. The reset is the slot's own, and the account-wide clean,
// which also sweeps the shared places, runs once every slot is idle
// (Clean), as on Linux. Uninstall deletes the account (DeleteAccount).
// Its Survey and Check are the machine's, as the account is: the survey
// works out the shared places, and the exposure check runs before a slot
// gets a runner (internal/exposure). Helper runs silkpool as the launcher
// each job runs, as the clean's kill helper and as the exposure check's
// helper.
//
// On Windows, each slot's runners run under the slot's service, in its
// slot host's job object (internal/winslot), as the machine's one
// service account (internal/winaccount). Slots keeps the account and
// checks it for drift (CheckAccount), readies a slot (Ready: the
// account, the slot's folders, the clean if it's due and the exposure
// check), and starts, stops, waits for and finds again its runners
// through the slots' services, on the slot hosts' copy for the agent's
// version (Start, Stop, Kill, Wait, Alive), looking for each job's worker
// in the slot's job object (WorkerUp). It resets them (Reset): a slot's
// own part ends its host's job, copies its runner's logs and deletes its
// runner root and temporary folder, and the account-wide clean, which
// disables the account, deletes its scheduled tasks, kills its
// processes, deletes its profile and empties its Recycle Bin, runs once
// every slot is idle (Clean), as on Linux and macOS. Its budget and its
// runners' memory go through the node job the agent keeps (Jobs).
package slot
