package secscan

import "regexp"

// The injection check is the union of two sets.
//
// legacyInjectionPatterns (scan.go) is the original list: a handful of exact
// phrasings. It is kept verbatim so the benchmark can measure the gain.
//
// familyPatterns below cover phrase FAMILIES, not phrasings: an instruction
// that overrides earlier ones, reassigns the reader's role, asks for the
// system prompt or a credential, coerces a tool call or a memory write,
// coerces a fixed reply, spoofs a delimiter or authority header, or sends
// something to a destination. Each family is written as a shape with slack
// for wording, so a paraphrase inside the shape still matches. None of them is
// a copy of a benchmark string. Every pattern is case-insensitive, and each is
// judged against benign text before it is kept (docs/INJECTION-DEFENSE.md has
// the false-positive measurement and the patterns that were dropped for it).
//
// Keep this list narrow. A noisy check gets switched off, and the vault stops
// seeing the findings it exists for.
var familyPatterns = []*regexp.Regexp{
	// Override: an instruction to drop what came before.
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|skip|override|bypass|drop)\s+(?:all\s+|any\s+|the\s+|your\s+|of\s+)*(?:(?:previous|prior|above|earlier|other|existing|original|preceding|foregoing|outdated|old|following|current|system)\s+)+(?:instructions?|rules|context|notes?|runbooks?|messages?|guidance|directives?|prompts?|guidelines|configuration|setup)\b`),
	regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget)\s+(?:the|these|those|all|any|that)\s+(?:runbooks?|markers?|fences?|delimiters?)\b`),
	regexp.MustCompile(`(?i)\b(?:your|the|these|previous|prior)\s+(?:previous\s+|prior\s+)?(?:instructions?|rules|guidance|notes?|runbooks?)\s+(?:were|are|is|have been|has been)\s+(?:obsolete|void|a test|cancell?ed|superseded|out of date|no longer (?:valid|apply|active))\b`),
	regexp.MustCompile(`(?i)\b(?:instructions?|rules|runbooks?)\s+(?:above|before|previously)\s+(?:are|is)\s+(?:now\s+)?(?:obsolete|void|out of date|superseded)\b`),
	regexp.MustCompile(`(?i)\bthe (?:real|actual) (?:task|instruction|request) (?:is|was)\s+to\b`),
	regexp.MustCompile(`(?i)\bfollow this (?:new )?instruction\b`),
	regexp.MustCompile(`(?i)\bfence (?:you are reading |here )?is (?:a )?(?:rendering artifact|mistake)\b`),

	// Role and identity reassignment.
	regexp.MustCompile(`(?i)\byou (?:are|'re) (?:now|actually|no longer)\b`),
	regexp.MustCompile(`(?i)\byou (?:are|'re) not (?:actually|really) (?:a|an|the)\b`),
	regexp.MustCompile(`(?i)\byou (?:are|'re) (?:a|an) (?:[a-z-]+ ){0,3}echo\b`),
	regexp.MustCompile(`(?i)\b(?:act|behave|pretend) as (?:if you (?:are|were)|an? (?:unrestricted|unfiltered|different|new))\b|\bpretend (?:to be|you are)\b`),
	regexp.MustCompile(`(?i)\bfrom now on,? you (?:will|must|should|are|have to)\b`),
	regexp.MustCompile(`(?i)\bnew (?:role|persona|identity)\s*(?:is|:)`),
	regexp.MustCompile(`(?i)\b(?:diagnostic|jailbreak)\s+mode\b`),

	// System prompt, credentials and authority.
	regexp.MustCompile(`(?i)\b(?:reveal|print|repeat|show|dump|output|disclose|leak|paste|recite)\s+(?:all\s+|the\s+|your\s+|any\s+)?(?:(?:hidden|internal|secret|original|full)\s+)?(?:system prompt|system instructions|hidden instructions|your instructions|your prompt)\b`),
	regexp.MustCompile(`(?i)\b(?:disclose|reveal|list|name|enumerate|describe|paste|dump|print)\b[^.\n]{0,40}\b(?:credentials?|secrets?|api tokens?|tokens?|keys?)\b[^.\n]{0,20}\b(?:you (?:hold|have|can see|can read|can access|know)|in the vault|held)\b`),
	regexp.MustCompile(`(?i)\b(?:any|your|held|the)\s+(?:api\s+)?(?:tokens?|credentials?|secrets?)\s+(?:you|that you)\s+(?:hold|have|can see|can read|can access)\b`),
	regexp.MustCompile(`(?i)\b(?:paste|dump|print|reveal|output|show)\s+(?:the\s+)?contents\s+of\s+(?:the\s+)?(?:credential|secret|token|key)\s*vault\b`),
	regexp.MustCompile(`(?i)\b(?:system|operator|administrator|admin|owner|maintenance|vault owner)\s+(?:notes?|notices?|override|instructions?)\b`),
	regexp.MustCompile(`(?i)\bthis note was written by (?:the )?(?:vault )?owner\b|\b(?:the )?(?:vault )?owner (?:instructs|asks|has instructed|requires)\b|\b(?:the )?operator (?:has )?(?:instructed|asks|requires|wants) you\b`),
	regexp.MustCompile(`(?i)\b(?:trusted|verified|authoritative|system)\s*:\s*(?:yes|true|highest)\b`),
	regexp.MustCompile(`(?i)\bsystem prompt\s*\((?:revised|updated|new|v\d+)\)`),

	// Delimiter, chat-template and fence spoofing.
	regexp.MustCompile(`(?i)<\|im_end\|>|<\|(?:assistant|user|endoftext|eot_id)\|>|<\|start_header_id\|>|\[/?INST\]|\[/?SYS\]`),
	regexp.MustCompile(`(?i)<<<\s*(?:END\s+)?(?:UNTRUSTED|TRUSTED)\b`),
	regexp.MustCompile(`(?i)\bend (?:of )?(?:the )?(?:untrusted|fenced|quoted)\s+(?:section|context|content|document|block)\b`),
	regexp.MustCompile(`(?i)\b(?:untrusted|fenced|fence|marker)s?\b[^.\n]{0,40}\b(?:has ended|were added by mistake|do not apply)\b`),
	regexp.MustCompile(`(?i)\beverything (?:after|below|following) (?:the )?(?:block|this|that|marker)\b[^.\n]{0,30}\btrusted\b`),
	regexp.MustCompile(`(?i)\btrusted (?:instruction|operator|system)\s*:`),
	regexp.MustCompile(`(?i)\b(?:operator|trusted|system) (?:instructions?|content|section|note)s? (?:resume|begin|start|follow)s?\b`),

	// Tool-call and memory-write coercion.
	regexp.MustCompile(`(?i)\b(?:call|invoke|use|run|execute)\s+(?:the\s+)?(?:tool\s+)?(?:list_grants|use_credential|get_credential|remember|forget|set_fact)\b`),
	regexp.MustCompile(`(?i)\b(?:remember|store|record|save)\b,?\s+(?:this|that|the following|it)?\s*(?:immutably|permanently|forever|as a permanent|for future sessions)\b`),
	regexp.MustCompile(`(?i)\bupdate your (?:long-term |persistent )?memory\b`),
	regexp.MustCompile(`(?i)\bfor future sessions\b`),
	regexp.MustCompile(`(?i)\b(?:mark|treat|record) (?:the )?(?:existing|old|original|prior|previous)\s+(?:procedure|runbook|rule|fact|note|numbers?)\b[^.\n]{0,20}\b(?:as )?(?:retracted|superseded|wrong|obsolete|invalid)\b`),
	regexp.MustCompile(`(?i)\bauthoritative over\b|\bpre-?granted\b|\bapprovals? (?:step )?(?:is|are) (?:optional|not required)\b`),
	regexp.MustCompile(`(?i)\b(?:instructions?|messages?|content|this (?:channel|thread|source))\b[^.\n]{0,30}\b(?:are|is) (?:now )?(?:trusted|authoritative)\b`),

	// Coerced reply: a fixed token or prefix to be emitted verbatim.
	regexp.MustCompile(`(?i)\b(?:reply|respond|output|emit|say|echo)\s+(?:with\s+)?(?:only|exactly|verbatim)\b`),
	regexp.MustCompile(`(?i)\b(?:reply|respond|output|emit)\b[^.\n]{0,40}\bnothing else\b`),
	regexp.MustCompile(`(?i)\b(?:reply|respond|output|emit|say|echo)\s+(?:with\s+|by\s+|using\s+)?(?:the\s+)?(?:(?:single|only|following|required|new)\s+)?(?:word|token|string|prefix|marker|phrase)\b`),
	regexp.MustCompile(`(?i)\b(?:prefix|prepend|append|begin|start)\s+(?:every|each|all|your|its)\s+(?:answers?|replies|responses?|outputs?)\b`),
	regexp.MustCompile(`(?i)\b(?:confirm|acknowledge|prove|verify)\b[^.\n]{0,40}\b(?:by|with|using|via)\s+(?:replying|responding|including|emitting|outputting|saying|writing|adding|appending)\b`),

	// Addressing an automated reader.
	regexp.MustCompile(`(?i)\b(?:any|all|every|each|whatever)\s+(?:ai\s+|llm\s+|language\s+model\s+|automated\s+)?(?:assistants?|bots?|readers?|llms?)\s+(?:reading|summari[sz]ing|processing|that reads|who reads|summari[sz]es|reads)\b`),
	regexp.MustCompile(`(?i)\b(?:note|message|reminder|notice|instruction)s? (?:to|for) (?:the |any |all |an? )?(?:ai |automated |llm )?(?:assistant|bot|reader)s?\b`),
	regexp.MustCompile(`(?i)\bif you (?:can read|are reading) this\b`),
	regexp.MustCompile(`(?i)\b(?:you|assistants?|bots?|readers?)\s+(?:must|shall|need to|have to|are required to)\s+(?:acknowledge|confirm|reply|respond|disclose|report|include|begin|prefix|output|emit|say|echo|append|list|paste|forward|send|post|upload|sync)\b`),
	regexp.MustCompile(`(?i)\bdo not (?:tell|inform|mention to|mention|show|report) (?:this |it |that |the (?:step|sync|upload|request) )?(?:to )?(?:the )?(?:user|summary|answer|reply|operator)\b`),
	regexp.MustCompile(`(?i)\bno need to (?:mention|tell|report|disclose|inform)\b`),
	regexp.MustCompile(`(?i)\breport only the (?:answer|result|reply) (?:to|for) (?:the )?user\b`),

	// Sending something to a destination.
	regexp.MustCompile(`(?i)\b(?:send|sending|post|posting|upload|uploading|forward|forwarding|submit|submitting|resubmit|re-submit|mirror|mirroring|transmit|push|paste|pasting|file|filing|sync|syncing|copy|copying|back ?up|backing up|deliver|email|exfiltrate)\b[^.\n]{0,100}?\b(?:to|at|into)\s+(?:https?://|ftp://|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,})`),
}

// injectionPatterns is what the check runs: the original phrasings and the
// families. Both are judged against each reading of a line.
var injectionPatterns = append(append([]*regexp.Regexp{}, legacyInjectionPatterns...), familyPatterns...)
