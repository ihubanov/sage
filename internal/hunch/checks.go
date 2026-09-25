package hunch

// The memory checks SAGE's write gate asks. Each names its look-alikes in
// yes_if / no_if. Wording is part of the contract: changing it changes the
// judge's accuracy, so ChecksVersion is recorded with every judgement and must
// be bumped whenever any wording below changes.
const ChecksVersion = "sage-memory-checks/2"

// Lasting asks whether a memory belongs in long-term memory at all. The
// failure it targets: an agent stores a remark about its own session ("the
// attachment was lost, ask the user to re-send the numbers") as a fact, and
// later sessions recall it as if it were true of the world. Standing rules and
// methods are legitimate lasting memories and are named as such, so procedural
// memories are not rejected for being instruction-shaped.
var Lasting = Check{
	Kind: "yesno",
	Question: "Should this be stored as lasting memory: does it state something about the world, " +
		"or a standing rule or method, that stays true outside the conversation it came from?",
	YesIf: "a fact about people, places, systems or events, or a standing rule, convention or method " +
		"a later reader should follow",
	NoIf: "a statement about the current conversation or session itself: a lost or unreadable message " +
		"or attachment, truncated context, something that could not be done just now, a request to re-send",
}

// Replaces asks whether `new` corrects `old`. Refinements ("adds detail") and
// corrections aimed at a different subject are the look-alikes that a plain
// "does new replace old?" check marks as replacements.
var Replaces = Check{
	Kind:     "yesno",
	Question: "Does `new` replace `old`'s value for the SAME thing?",
	YesIf: "`new` is about the same subject, attribute and period as `old` and states a value that " +
		"contradicts or corrects it",
	NoIf: "`new` restates the same value in other words or units; only adds detail without contradicting " +
		"`old`; or is about a different subject, place, period or attribute, even if it is worded as a correction",
}

// Agrees asks whether two memories assert the same value for the same thing
// (a semantic duplicate, which the content-hash dedup cannot see). A duplicate
// verdict links the two into one restatement class, so a correction of either
// hides both: a refinement wrongly called a duplicate would be hidden with its
// original. The look-alike named here is therefore "adds detail" (v2; the
// generic v1 wording called refinements duplicates at p=1.0 on some models).
var Agrees = Check{
	Kind:     "yesno",
	Question: "Do `a` and `b` assert the SAME value for the same thing, with nothing added?",
	YesIf:    "same thing and same value, only worded differently or in other units",
	NoIf: "a different or changed value, a different thing, or `b` adds detail or conditions that `a` " +
		"does not state (a refinement is not a duplicate)",
}

// Supported asks whether a memory is backed by the evidence submitted with it.
// It is only ever asked WITH the evidence in context: without it the question
// degrades into "does this sound plausible", which is not a confidence.
var Supported = Check{
	Kind:     "yesno",
	Question: "Is the assertion in `memory` supported by `evidence`?",
	YesIf:    "`evidence` states or directly shows what `memory` asserts",
	NoIf: "`evidence` is about something else, only makes it plausible, contradicts it, " +
		"or supports a weaker or different claim than `memory` makes",
}
