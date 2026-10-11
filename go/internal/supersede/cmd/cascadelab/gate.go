package main

// The gate question and the Jev labelling pass. Every Jev call is appended to
// the ledger BEFORE it is made; the run refuses once the ledger holds the cap.
// Answers are cached by pair id, so re-running never pays twice for a pair.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cascade"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/supersede"
)

// jevCap is the hard budget of Jev calls for the whole experiment.
const jevCap = 2500

// gateQuestion is the shared gate question (internal/cascade).
var gateQuestion = cascade.Question()

// gateState renders the pair as the state the question is asked about.
func gateState(p supersede.Pair) string { return cascade.State(p.Old, p.New) }

// Answer is one cached gate answer.
type Answer struct {
	ID      string  `json:"id"`
	Split   string  `json:"split"`
	Replace float64 `json:"p_replace"`
	Label   string  `json:"label"`
	Model   string  `json:"model"`
}

// Ledger is the append-only record of Jev calls.
type Ledger struct {
	mu    sync.Mutex
	path  string
	count int
}

// OpenLedger counts the calls already recorded.
func OpenLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path}
	f, err := os.Open(path)
	if err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if len(sc.Bytes()) > 0 {
				l.count++
			}
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return l, nil
}

// Reserve records one call. It refuses when the cap is reached.
func (l *Ledger) Reserve(pairID, split, purpose string) error {
	if l == nil {
		return nil // scoring a local model makes no Jev call
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count >= jevCap {
		return errors.New("jev budget exhausted")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{"seq": l.count + 1, "time": time.Now().UTC().Format(time.RFC3339),
		"pair": pairID, "split": split, "purpose": purpose, "model": "jev-latest"})
	if _, err := f.Write(append(rec, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	l.count++
	return nil
}

// Count returns the calls recorded so far.
func (l *Ledger) Count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// loadCache reads cached gate answers keyed by pair id.
func loadCache(path string) (map[string]Answer, error) {
	out := map[string]Answer{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var a Answer
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, sc.Err()
}

func appendCache(path string, a Answer) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(a)
	_, err = f.Write(append(b, '\n'))
	return err
}

// askGate asks one decision server about one pair.
func askGate(ctx context.Context, c *decide.Client, p supersede.Pair) (Answer, error) {
	res, err := c.Ask(ctx, gateState(p), gateQuestion)
	if err != nil {
		return Answer{}, err
	}
	a, ok := res.Answers["replace"]
	if !ok || a.Type != decide.Choice {
		return Answer{}, fmt.Errorf("no choice answer for %s", p.ID)
	}
	pr, has := a.Probabilities["replace"]
	if !has && a.Choice == "replace" {
		pr = 1 // a server that reports only a label: a hard vote
	}
	return Answer{ID: p.ID, Split: p.Split, Replace: pr, Label: a.Choice, Model: res.Model}, nil
}

// runJev labels every pair in pairs that is not cached, with the given client,
// through the ledger. Failures are reported and not cached.
func runJev(c *decide.Client, pairs []supersede.Pair, cachePath string, ledger *Ledger, workers int) (int, int, error) {
	cache, err := loadCache(cachePath)
	if err != nil {
		return 0, 0, err
	}
	var todo []supersede.Pair
	for _, p := range pairs {
		if _, ok := cache[p.ID]; !ok {
			todo = append(todo, p)
		}
	}
	var (
		mu       sync.Mutex
		done     int
		failed   int
		wg       sync.WaitGroup
		jobs     = make(chan supersede.Pair)
		writeErr error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				if err := ledger.Reserve(p.ID, p.Split, "gate"); err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				a, err := askGate(ctx, c, p)
				cancel()
				mu.Lock()
				if err != nil {
					failed++
					fmt.Fprintln(os.Stderr, "jev:", p.ID, err)
				} else if werr := appendCache(cachePath, a); werr != nil {
					writeErr = werr
				} else {
					done++
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range todo {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	return done, failed, writeErr
}
