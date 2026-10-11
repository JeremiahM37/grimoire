// Package supersede is the measurement harness for the `cascade` experiment: a
// deterministic generator of labelled (old fact, new fact) pairs whose truth is
// known to the generator, and the evaluation of the write-time rules against
// it. The pairs themselves are generated at run time and written only to the
// lab work directory; this package holds no real vault text.
//
// Truth. Supersede is true when the new statement is a newer value of the same
// attribute of the same subject, so the old one should be struck through
// (true update, paraphrased update). It is false when both can be true at once
// (refinement, modifier collision, variant collision, sibling attribute,
// unrelated vocabulary, list membership) or when nothing changed (same text).
//
// Splits. Templates are drawn from three disjoint banks, one per split, and the
// subject pools are disjoint too, so no sentence frame and no subject is shared
// between train, calibration and test.
package supersede

import (
	"fmt"
	"math/rand"
	"strings"
)

// Categories, in report order.
const (
	CatTrueUpdate     = "true_update"
	CatParaUpdate     = "paraphrased_update"
	CatRefinement     = "refinement"
	CatSameText       = "same_text"
	CatModifier       = "modifier_collision"
	CatVariant        = "variant_collision"
	CatSibling        = "sibling_attribute"
	CatUnrelatedVocab = "unrelated_vocab"
	CatListEdge       = "list_edge"
)

// Categories lists every category in report order.
var Categories = []string{CatTrueUpdate, CatParaUpdate, CatRefinement, CatSameText,
	CatModifier, CatVariant, CatSibling, CatUnrelatedVocab, CatListEdge}

// Splits.
const (
	Train = "train"
	Calib = "calib"
	Test  = "test"
)

// Pair is one labelled (old, new) statement pair.
type Pair struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	Split     string `json:"split"`
	Domain    string `json:"domain"`
	Old       string `json:"old"`
	New       string `json:"new"`
	Supersede bool   `json:"supersede"`
}

// Pool is one split's sentence frames and subjects.
type pool struct {
	subjects []string
	people   []string
	cities   []string
	tools    []string
	heads    []string
	mods     []string
	variants [][2]string
	prefs    [][2]string
	quals    []string
}

// The subjects are split into three disjoint pools, one per split.
var pools = map[string]pool{
	Train: {
		subjects: []string{"the billing ledger job", "the nightly backup", "the deploy pipeline",
			"the embedding batch", "the search reindex", "the metrics collector", "the mail relay",
			"the cache warmer", "the image pipeline"},
		people:   []string{"Dana", "Priya", "Marcus"},
		cities:   []string{"Denver", "Lisbon", "Osaka", "Toronto", "Berlin", "Austin"},
		tools:    []string{"postgres", "redis", "sqlite", "nginx", "haproxy"},
		heads:    []string{"replica", "queue", "worker", "cluster", "index"},
		mods:     []string{"billing", "analytics", "staging", "production", "edge", "archive"},
		variants: [][2]string{{"bf16", "fp32"}, {"v1", "v2"}, {"cpu", "gpu"}, {"sqlite", "postgres"}},
		prefs:    [][2]string{{"tabs", "spaces"}, {"vim", "emacs"}, {"dark", "light"}, {"metric", "imperial"}},
		quals:    []string{"with connection pooling", "in read-only mode", "for reporting queries", "behind the proxy", "on weekdays only"},
	},
	Calib: {
		subjects: []string{"the sync worker", "the thumbnail service", "the audit exporter",
			"the photo indexer", "the calendar bridge", "the status page", "the log shipper",
			"the feed poller", "the reminder cron"},
		people:   []string{"Ines", "Tomas", "Kenji"},
		cities:   []string{"Porto", "Kyoto", "Quebec", "Seattle", "Leeds", "Turin"},
		tools:    []string{"mysql", "memcached", "etcd", "traefik", "caddy"},
		heads:    []string{"mirror", "shard", "consumer", "gateway", "store"},
		mods:     []string{"ingest", "reporting", "canary", "primary", "legacy", "regional"},
		variants: [][2]string{{"int8", "fp16"}, {"beta", "stable"}, {"x86", "arm"}, {"ssd", "hdd"}},
		prefs:    [][2]string{{"zsh", "fish"}, {"light", "dark"}, {"tea", "coffee"}, {"rust", "go"}},
		quals:    []string{"with retries enabled", "during business hours", "as a background task", "behind a feature flag"},
	},
	Test: {
		subjects: []string{"the review bot", "the invoice sender", "the crawl scheduler",
			"the vector rebuild", "the webhook relay", "the nightly lint", "the release tagger",
			"the chart renderer", "the token rotator"},
		people:   []string{"Hana", "Omar", "Lucia"},
		cities:   []string{"Dublin", "Nairobi", "Oslo", "Montreal", "Recife", "Hobart"},
		tools:    []string{"kafka", "minio", "vault", "grafana", "loki"},
		heads:    []string{"tenant", "bucket", "topic", "dashboard", "stream"},
		mods:     []string{"eu", "us", "beta", "internal", "public", "scratch"},
		variants: [][2]string{{"q4", "q8"}, {"gen3", "gen4"}, {"ipv4", "ipv6"}, {"dev", "prod"}},
		prefs:    [][2]string{{"emacs", "helix"}, {"pytest", "unittest"}, {"yaml", "toml"}, {"spaces", "tabs"}},
		quals:    []string{"with a local cache", "for batch jobs only", "in shadow mode", "across regions"},
	},
}

// Category frames: for each split, a bank of sentence frames. {S} is the
// subject, {V} a value, {P} a person, {C} a city, {T} a tool. Frames within one
// split are distinct from every frame in another split.

var valueFrames = map[string]map[string][]string{
	// minutes
	"min": {
		Train: {"{S} takes {V} minutes", "{S} runs for {V} minutes", "{S} needs {V} minutes"},
		Calib: {"{S} finishes in {V} minutes", "{S} is done after {V} minutes"},
		Test:  {"{S} wall clock is {V} minutes", "{S} needs roughly {V} minutes"},
	},
	// port
	"port": {
		Train: {"{S} listens on port {V}", "{S} binds port {V}"},
		Calib: {"{S} is served on port {V}", "{S} accepts traffic on port {V}"},
		Test:  {"{S} exposes port {V}", "{S} is reachable on port {V}"},
	},
	// city
	"city": {
		Train: {"{P} lives in {C}", "{P} is based in {C}"},
		Calib: {"{P} moved to {C}", "{P} now calls {C} home"},
		Test:  {"{P} works out of {C}", "{P} is located in {C}"},
	},
	// clock
	"clock": {
		Train: {"{S} starts at {V}", "{S} is scheduled for {V}"},
		Calib: {"{S} kicks off at {V}", "{S} runs at {V} each day"},
		Test:  {"{S} is set to run at {V}", "{S} begins at {V} daily"},
	},
	// count
	"count": {
		Train: {"{S} has {V} open items", "{S} is tracking {V} items"},
		Calib: {"{S} holds {V} open entries", "{S} carries {V} pending items"},
		Test:  {"{S} now has {V} open tickets", "{S} counts {V} outstanding items"},
	},
	// version
	"ver": {
		Train: {"{T} runs version {V}", "{T} is pinned to {V}"},
		Calib: {"{T} is on release {V}", "{T} ships build {V}"},
		Test:  {"{T} is installed at {V}", "{T} is at revision {V}"},
	},
}

// Paraphrase pairs: two frames that say the same kind of attribute with
// different verbs and wording. Per split.
var paraFrames = map[string][][2]string{
	Train: {
		{"{S} takes {V} minutes", "{S} now finishes in {V} minutes"},
		{"{S} listens on port {V}", "{S} was moved so it answers on {V}"},
		{"{P} lives in {C}", "{P} relocated, and {C} is home now"},
	},
	Calib: {
		{"{S} needs {V} minutes", "{S} ends after {V} minutes these days"},
		{"{S} is served on port {V}", "{S} was switched to answer on port {V}"},
		{"{P} moved to {C}", "{P} is living in {C} at the moment"},
	},
	Test: {
		{"{S} runs for {V} minutes", "{S} completes in {V} minutes after the change"},
		{"{S} binds port {V}", "{S} was reconfigured onto port {V}"},
		{"{P} is based in {C}", "{P} has since set up in {C}"},
	},
}

// Sibling and modifier frames per split (two attributes of one subject; two
// subjects that share a head noun). {M} modifier, {H} head, {V} value.
var siblingFrames = map[string][][2]string{
	Train: {
		{"the {H} login shell is {V}", "the {H} ssh shell is {W}"},
		{"{S} listens on port {V}", "{S} serves TLS on port {W}"},
	},
	Calib: {
		{"the {H} default editor is {V}", "the {H} default pager is {W}"},
		{"{S} accepts jobs on port {V}", "{S} publishes metrics on port {W}"},
	},
	Test: {
		{"the {H} boot disk is {V}", "the {H} swap device is {W}"},
		{"{S} exposes an api on port {V}", "{S} exposes a admin console on port {W}"},
	},
}

var modifierFrames = map[string][]string{
	Train: {"the {M1} {H} lags {V} seconds", "the {M1} {H} keeps {V} rows"},
	Calib: {"the {M1} {H} is at {V} percent", "the {M1} {H} waits {V} seconds"},
	Test:  {"the {M1} {H} holds {V} jobs", "the {M1} {H} is sized to {V} gigabytes"},
}

var variantFrames = map[string][]string{
	Train: {"the {H} with {A} weights runs at {V} tokens per second", "the {H} built for {A} reaches {V} requests per second"},
	Calib: {"the {H} on {A} handles {V} requests per second", "the {H} using {A} stores {V} records per minute"},
	Test:  {"the {H} in its {A} variant takes {V} seconds per batch", "the {H} for the {A} tier returns {V} results per query"},
}

var unrelatedFrames = map[string][]string{
	Train: {"the {H} takes {V} minutes end to end", "the {H} needs {V} minutes end to end"},
	Calib: {"the {H} runs {V} minutes end to end", "the {H} finishes in {V} minutes end to end"},
	Test:  {"the {H} takes {V} minutes from start to finish", "the {H} costs {V} minutes end to end"},
}

var listFrames = map[string][]string{
	Train: {"{P} is allergic to {A}", "{P} avoids {A}"},
	Calib: {"{P} has an allergy to {A}", "{P} must not eat {A}"},
	Test:  {"{P} reacts badly to {A}", "{P} keeps away from {A}"},
}

var allergens = []string{"peanuts", "shellfish", "gluten", "sesame", "tree nuts", "eggs", "soy", "kiwi", "latex", "pollen"}

// Generate returns n pairs per category per split (so 9*3*n in total), in a
// fixed order, deterministic for a seed.
func Generate(seed int64, perCatSplit int) []Pair {
	rng := rand.New(rand.NewSource(seed))
	var out []Pair
	id := 0
	for _, cat := range Categories {
		for _, split := range []string{Train, Calib, Test} {
			for i := 0; i < perCatSplit; i++ {
				id++
				p := build(rng, cat, split)
				p.ID = fmt.Sprintf("p%05d", id)
				p.Category, p.Split = cat, split
				out = append(out, p)
			}
		}
	}
	return out
}

func build(rng *rand.Rand, cat, split string) Pair {
	pl := pools[split]
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	subj := pick(pl.subjects)
	person := pick(pl.people)
	domain := map[string]string{Train: "ops", Calib: "measure", Test: "schedule"}[split]
	kinds := []string{"min", "port", "city", "clock", "count", "ver"}
	kind := kinds[rng.Intn(len(kinds))]
	v1, v2 := distinct(rng, func() string { return valueOf(rng, kind) })
	fr := valueFrames[kind][split]
	fill := func(f string, v string) string {
		r := strings.NewReplacer("{S}", subj, "{P}", person, "{C}", pick(pl.cities),
			"{T}", pick(pl.tools), "{V}", v)
		return r.Replace(f)
	}
	switch cat {
	case CatTrueUpdate:
		f := fr[rng.Intn(len(fr))]
		c := pick(pl.cities)
		o := strings.NewReplacer("{S}", subj, "{P}", person, "{C}", c, "{T}", pl.tools[0], "{V}", v1).Replace(f)
		n := strings.NewReplacer("{S}", subj, "{P}", person, "{C}", c, "{T}", pl.tools[0], "{V}", v2).Replace(f)
		if kind == "city" {
			n = strings.NewReplacer("{S}", subj, "{P}", person, "{C}", otherCity(rng, pl, c), "{T}", pl.tools[0], "{V}", v2).Replace(f)
		}
		return Pair{Domain: domain, Old: o + ".", New: n + ".", Supersede: true}
	case CatParaUpdate:
		pf := paraFrames[split][rng.Intn(len(paraFrames[split]))]
		c := pick(pl.cities)
		c2 := otherCity(rng, pl, c)
		vv1, vv2 := v1, v2
		if strings.Contains(pf[0], "{C}") {
			vv1, vv2 = c, c2
		}
		o := strings.NewReplacer("{S}", subj, "{P}", person, "{C}", c, "{V}", vv1).Replace(pf[0])
		n := strings.NewReplacer("{S}", subj, "{P}", person, "{C}", c2, "{V}", vv2).Replace(pf[1])
		return Pair{Domain: domain, Old: o + ".", New: n + ".", Supersede: true}
	case CatRefinement:
		f := fr[rng.Intn(len(fr))]
		base := fill(f, v1)
		return Pair{Domain: domain, Old: base + ".", New: base + " " + pick(pl.quals) + ".", Supersede: false}
	case CatSameText:
		base := fill(fr[rng.Intn(len(fr))], v1)
		switch rng.Intn(3) {
		case 0:
			return Pair{Domain: domain, Old: base + ".", New: base + ".", Supersede: false}
		case 1:
			return Pair{Domain: domain, Old: base + ".", New: strings.ToUpper(base[:1]) + base[1:] + "", Supersede: false}
		default:
			return Pair{Domain: domain, Old: base, New: base + ".", Supersede: false}
		}
	case CatModifier:
		mf := modifierFrames[split][rng.Intn(len(modifierFrames[split]))]
		m1, m2 := distinct(rng, func() string { return pick(pl.mods) })
		head := pick(pl.heads)
		_ = m2
		n1, n2 := distinct(rng, func() string { return numeric(rng) })
		o := strings.NewReplacer("{M1}", m1, "{H}", head, "{V}", n1).Replace(mf)
		n := strings.NewReplacer("{M1}", m2, "{H}", head, "{V}", n2).Replace(mf)
		return Pair{Domain: domain, Old: o + ".", New: n + ".", Supersede: false}
	case CatVariant:
		vf := pick2(rng, variantFrames[split])
		pv := pl.variants[rng.Intn(len(pl.variants))]
		head := pick(pl.heads)
		n1, n2 := distinct(rng, func() string { return numeric(rng) })
		o := strings.NewReplacer("{H}", head, "{A}", pv[0], "{V}", n1).Replace(vf)
		n := strings.NewReplacer("{H}", head, "{A}", pv[1], "{V}", n2).Replace(vf)
		return Pair{Domain: domain, Old: o + ".", New: n + ".", Supersede: false}
	case CatSibling:
		sf := siblingFrames[split][rng.Intn(len(siblingFrames[split]))]
		head := pick(pl.heads)
		// The two statements are different attributes of one subject; each
		// holds its own value. Port frames take ports, the rest take names.
		var a, b string
		if strings.Contains(sf[0], "port") {
			a, b = distinct(rng, func() string { return fmt.Sprint(1024 + rng.Intn(60000)) })
		} else {
			a, b = distinct(rng, func() string { return pick(pl.tools) })
		}
		o := strings.NewReplacer("{H}", head, "{S}", subj, "{V}", a).Replace(sf[0])
		n := strings.NewReplacer("{H}", head, "{S}", subj, "{W}", b).Replace(sf[1])
		return Pair{Domain: domain, Old: o + ".", New: n + ".", Supersede: false}
	case CatUnrelatedVocab:
		uf := pick2(rng, unrelatedFrames[split])
		h1, h2 := distinct(rng, func() string { return pick(pl.subjects) })
		n1, n2 := distinct(rng, func() string { return numeric(rng) })
		o := strings.NewReplacer("{H}", strings.TrimPrefix(h1, "the "), "{V}", n1).Replace(uf)
		n := strings.NewReplacer("{H}", strings.TrimPrefix(h2, "the "), "{V}", n2).Replace(uf)
		return Pair{Domain: domain, Old: o + ".", New: n + ".", Supersede: false}
	case CatListEdge:
		lf := pick2(rng, listFrames[split])
		a1, a2 := distinct(rng, func() string { return allergens[rng.Intn(len(allergens))] })
		o := strings.NewReplacer("{P}", person, "{A}", a1).Replace(lf)
		n := strings.NewReplacer("{P}", person, "{A}", a2).Replace(lf)
		return Pair{Domain: domain, Old: capFirst(o) + ".", New: capFirst(n) + ".", Supersede: false}
	}
	panic("unknown category " + cat)
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// numeric is a plain count of minutes, seconds or units.
func numeric(rng *rand.Rand) string { return fmt.Sprint(2 + rng.Intn(120)) }

func pick2(rng *rand.Rand, xs []string) string { return xs[rng.Intn(len(xs))] }

// distinct draws two different values from gen.
func distinct(rng *rand.Rand, gen func() string) (string, string) {
	a := gen()
	for i := 0; i < 100; i++ {
		b := gen()
		if b != a {
			return a, b
		}
	}
	return a, a + "x"
}

func otherCity(rng *rand.Rand, pl pool, c string) string {
	for {
		x := pl.cities[rng.Intn(len(pl.cities))]
		if x != c {
			return x
		}
	}
}

// valueOf draws one typed literal for a kind.
func valueOf(rng *rand.Rand, kind string) string {
	switch kind {
	case "min":
		return fmt.Sprint(5 + rng.Intn(115))
	case "port":
		return fmt.Sprint(1024 + rng.Intn(60000))
	case "city":
		return "x"
	case "clock":
		return fmt.Sprintf("%d:%02d", 6+rng.Intn(14), []int{0, 15, 30, 45}[rng.Intn(4)])
	case "count":
		if rng.Intn(3) == 0 {
			return []string{"two", "three", "four", "five", "six", "seven", "eight", "nine"}[rng.Intn(8)]
		}
		return fmt.Sprint(2 + rng.Intn(60))
	case "ver":
		return fmt.Sprintf("%d.%d", 10+rng.Intn(8), rng.Intn(10))
	}
	return "1"
}
