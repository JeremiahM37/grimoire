package memory

import "strings"

// ContextStopwords are the words context ranking does not count as overlap
// between a request and a memory: function words and the filler of a
// request ("can you fix the thing"). Matching on them made unrelated text
// look relevant. The context endpoint and memory replay share this list.
var ContextStopwords = strings.Fields("please can could would should will do does did how what when where why which who me my we our you your it this that these those help want need now just also really anything something tell explain use using work working fix add make get know thanks thank okay ok yes no continue proceed hello hi " +
	"the and for are was were has have had with from then than there their they them all any some been into only over about after before its not but out off too very more most much many such same other each every both being here been let lets got going gonna wanna thing things stuff way ones one sure still again even well like look see try")
