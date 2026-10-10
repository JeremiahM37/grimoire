package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/grounded"
)

// grimoire ground — the grounded-answer pieces over a corpus file, with no
// vault. Used to measure the feature on a fixed corpus and to run it on any
// set of dated records:
//
//	ground prepare   annotate relative times in every record (no model)
//	ground timeline  extract the entity/event timeline (model)
//	ground answer    answer a batch of questions from prepared corpora
const groundUsage = `grimoire ground — grounded answers over dated records

  grimoire ground prepare  --docs docs.json --out prepared.json
        annotate relative times ("last Saturday") with absolute dates; the
        original text is kept and the annotations are stored beside it
  grimoire ground timeline --in prepared.json [--out prepared.json] [--model M] [--workers 4]
        extract the per-entity dated-event timeline with the memory bank's extractor
  grimoire ground render   --in prepared.json [--annotate]
        print the records as a reader would see them
  grimoire ground answer   --dir DIR --questions q.jsonl --out a.jsonl [--model M] [--parallel 4]
                           [--annotate] [--timeline] [--procedure] [--direct-template FILE]
        answer questions (lines of {"qid","corpus","question"}); DIR holds <corpus>.json

Model calls go through the claude command (llm=claude-cli).`

func groundClient(model string) *ai.Client {
	return ai.New(mapSettings{"llm": ai.BackendClaudeCLI, "llm_model": model}, nil)
}

type mapSettings map[string]string

func (m mapSettings) Get(k string) string { return m[k] }

func cmdGround(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, groundUsage)
		return 2
	}
	switch args[0] {
	case "prepare":
		return groundPrepare(args[1:])
	case "timeline":
		return groundTimeline(args[1:])
	case "answer":
		return groundAnswer(args[1:])
	case "render":
		in, _ := flagValue(args[1:], "--in")
		p, err := grounded.LoadPrepared(in)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println(p.Render(grounded.RenderOpts{Annotate: hasFlag(args, "--annotate")}))
		return 0
	}
	return fail("unknown ground subcommand %q\n%s", args[0], groundUsage)
}

func groundPrepare(args []string) int {
	in, _ := flagValue(args, "--docs")
	out, _ := flagValue(args, "--out")
	if in == "" || out == "" {
		return fail("ground prepare needs --docs and --out")
	}
	docs, err := grounded.LoadDocs(in)
	if err != nil {
		return fail("%v", err)
	}
	p := grounded.Prepare(context.Background(), docs, nil)
	n := 0
	for _, d := range p.Docs {
		for _, a := range d.Annotations {
			n += len(a)
		}
	}
	if err := p.Save(out); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("prepared %d documents, %d time expressions annotated\n", len(p.Docs), n)
	return 0
}

func groundTimeline(args []string) int {
	in, _ := flagValue(args, "--in")
	out, ok := flagValue(args, "--out")
	if !ok {
		out = in
	}
	model, ok := flagValue(args, "--model")
	if !ok {
		model = "claude-sonnet-5-5"
	}
	workers := 4
	if v, ok := flagValue(args, "--workers"); ok {
		workers, _ = strconv.Atoi(v)
	}
	if in == "" {
		return fail("ground timeline needs --in")
	}
	p, err := grounded.LoadPrepared(in)
	if err != nil {
		return fail("%v", err)
	}
	docs := make([]grounded.Doc, len(p.Docs))
	for i, d := range p.Docs {
		docs[i] = d.Doc
	}
	tl, err := grounded.BuildTimeline(context.Background(), docs, grounded.BankExtractor(groundClient(model)), workers, "",
		func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
		if len(tl.Events) == 0 {
			return 1
		}
	}
	p.Timeline = tl
	if err := p.Save(out); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("timeline: %d events from %d documents\n", len(tl.Events), len(docs))
	return 0
}

type groundQ struct {
	QID      string `json:"qid"`
	Corpus   string `json:"corpus"`
	Question string `json:"question"`
}

func groundAnswer(args []string) int {
	dir, _ := flagValue(args, "--dir")
	qf, _ := flagValue(args, "--questions")
	outf, _ := flagValue(args, "--out")
	model, ok := flagValue(args, "--model")
	if !ok {
		model = "claude-sonnet-5-5"
	}
	par := 4
	if v, ok := flagValue(args, "--parallel"); ok {
		par, _ = strconv.Atoi(v)
	}
	if par > 4 {
		par = 4 // the protocol caps parallelism
	}
	if dir == "" || qf == "" || outf == "" {
		return fail("ground answer needs --dir, --questions and --out")
	}
	opt := grounded.Options{Procedure: hasFlag(args, "--procedure"), Timeline: hasFlag(args, "--timeline"),
		System: "You answer questions strictly from the supplied records."}
	if f, ok := flagValue(args, "--direct-template"); ok {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fail("%v", err)
		}
		opt.DirectTemplate = string(raw)
	}
	if s, ok := flagValue(args, "--system"); ok {
		opt.System = s
	}
	annotate := hasFlag(args, "--annotate")

	done := map[string]bool{}
	if f, err := os.Open(outf); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			var r struct {
				QID string `json:"qid"`
			}
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				done[r.QID] = true
			}
		}
		f.Close()
	}
	var qs []groundQ
	f, err := os.Open(qf)
	if err != nil {
		return fail("%v", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var q groundQ
		if json.Unmarshal(sc.Bytes(), &q) == nil && q.QID != "" && !done[q.QID] {
			qs = append(qs, q)
		}
	}
	f.Close()

	corpora := map[string]*grounded.Prepared{}
	var cmu sync.Mutex
	load := func(name string) (*grounded.Prepared, error) {
		cmu.Lock()
		defer cmu.Unlock()
		if p, ok := corpora[name]; ok {
			return p, nil
		}
		p, err := grounded.LoadPrepared(filepath.Join(dir, name+".json"))
		if err == nil {
			corpora[name] = p
		}
		return p, err
	}
	out, err := os.OpenFile(outf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fail("%v", err)
	}
	defer out.Close()
	ans := &grounded.Answerer{LLM: grounded.ClientLLM{C: groundClient(model)}, Opt: opt}
	fmt.Fprintf(os.Stderr, "%d questions to answer (%d done) annotate=%v timeline=%v procedure=%v\n", len(qs), len(done), annotate, opt.Timeline, opt.Procedure)

	var wmu sync.Mutex
	var fails, consecutive, n atomic.Int64
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for _, q := range qs {
		if consecutive.Load() >= 8 {
			fmt.Fprintln(os.Stderr, "stopping: 8 consecutive failures")
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(q groundQ) {
			defer wg.Done()
			defer func() { <-sem }()
			p, err := load(q.Corpus)
			var res grounded.Result
			if err == nil {
				res, err = ans.Answer(context.Background(), q.Question, grounded.WholeSource{P: p, Annotate: annotate})
			}
			if err != nil {
				fails.Add(1)
				consecutive.Add(1)
				fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", q.QID, err)
				return
			}
			consecutive.Store(0)
			raw, _ := json.Marshal(map[string]any{"qid": q.QID, "answer": res.Answer, "evidence": res.Evidence, "check": res.Check,
				"calls": res.Calls, "in_tokens": res.In, "out_tokens": res.Out, "cost_usd": res.CostUSD, "ms": res.Millis, "timeline_events": res.Events})
			wmu.Lock()
			out.Write(append(raw, '\n'))
			wmu.Unlock()
			if k := n.Add(1); k%25 == 0 {
				fmt.Fprintln(os.Stderr, k)
			}
		}(q)
	}
	wg.Wait()
	fmt.Printf("answered %d, failed %d\n", n.Load(), fails.Load())
	if fails.Load() > 0 {
		return 1
	}
	return 0
}
