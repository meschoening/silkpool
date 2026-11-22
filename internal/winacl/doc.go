// Package winacl checks a Windows security descriptor against the exact
// one Silkpool gave an object: its owner, and a DACL that's present,
// protected from inheritance, and exactly the allow entries it set. The
// agent's pipe and its folders are made this way, so anything else is
// someone else's object, or one someone reshaped. Checking the whole
// DACL, rather than looking for one bad right, leaves nothing to reason
// about in grants, denials and generic rights. Decided in "Build the
// Windows agent service and its named-pipe control channel".
package winacl
