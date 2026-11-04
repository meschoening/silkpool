package wording

// The node CLI's words for an enroll whose answer was lost, from "Retry
// a lost enrollment answer once, then tell the operator".

// EnrollAnswerLost follows an enroll that failed because the Hub's
// answer was lost after the enroll went out, and again on the agent's
// one retry. The Hub may have enrolled the machine, but a rerun with the
// same code reuses the pending node key, so the Hub answers it as the
// node it enrolled, as "Make enrollment safe to retry, check node names
// and prune old codes"
// decided.
const EnrollAnswerLost = "Run `sudo silkpool enroll <code>` again with the same code. That's safe: " +
	"the machine joins as one node, whether or not the Hub enrolled it already."
