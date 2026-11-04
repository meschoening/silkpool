package node

import "errors"

// ErrHubIdentityChanged refuses a Hub that presents a key other than the
// one the node pinned. A node whose Hub's key really changed is enrolled
// again.
var ErrHubIdentityChanged = errors.New("the Hub's key doesn't match the one its enrollment code pinned")

// A Pin is the key a node trusts its Hub by when the Hub serves its own
// self-signed certificate: the one its enrollment code pinned, kept with
// the Hub's URL beside the node. A Hub with a publicly trusted
// certificate is verified through the system's trusted roots instead,
// never through a Pin.
type Pin string

// Check refuses a Hub that presents key unless key is the pinned one.
// There's no override, and a pin is never replaced: the empty Pin
// accepts no key.
func (p Pin) Check(key string) error {
	if key == "" || key != string(p) {
		return ErrHubIdentityChanged
	}
	return nil
}
