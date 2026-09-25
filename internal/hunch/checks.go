package hunch

// The check SAGE's memory gate asks. It names its look-alikes in yes_if /
// no_if; the wording is part of the contract (changing it changes the judge's
// accuracy), so ChecksVersion is recorded with every judgement and must be
// bumped whenever the wording changes.
const ChecksVersion = "sage-lasting/1"

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
