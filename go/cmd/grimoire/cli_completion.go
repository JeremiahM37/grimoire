package main

import (
	"fmt"
	"sort"
	"strings"
)

// Shell completion and "did you mean" for the command word.
//
// Both read the command table, so neither can list a command that does not
// dispatch or miss one that does. Completion covers subcommand names only;
// that is what a new user reaches for, and flags are better read from help.

// dispatchWords are the words runCLI answers before the table: serve falls
// through to the server, and help and version are handled up front. They are
// completable and suggestible like any command.
var dispatchWords = []string{"help", "serve", "version"}

// commandWords returns every word that can follow "grimoire", sorted.
func commandWords() []string {
	seen := map[string]bool{}
	var out []string
	for name := range commands() {
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range dispatchWords {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// suggestCommand returns the closest command word to word, or "" when nothing
// is within two edits. Ties go to the alphabetically first name, because the
// candidates are sorted.
func suggestCommand(word string, names []string) string {
	best, bestDist := "", 3
	for _, name := range names {
		if d := levenshtein(word, name); d < bestDist {
			best, bestDist = name, d
		}
	}
	return best
}

// levenshtein is the edit distance between two strings, counted in runes.
// Two rolling rows are enough: only the previous row is ever read.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func cmdCompletion(args []string) int {
	const want = "usage: grimoire completion bash|zsh|fish"
	if len(args) != 1 {
		return fail(want)
	}
	words := strings.Join(commandWords(), " ")
	switch args[0] {
	case "bash":
		fmt.Print(bashCompletion(words))
	case "zsh":
		fmt.Print(zshCompletion(words))
	case "fish":
		fmt.Print(fishCompletion(words))
	default:
		return fail("%s, not %q", want, args[0])
	}
	return 0
}

// bashCompletion completes the command word after "grimoire".
func bashCompletion(words string) string {
	return `# bash completion for grimoire: source <(grimoire completion bash)
_grimoire_complete() {
    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "` + words + `" -- "${COMP_WORDS[COMP_CWORD]}") )
    fi
}
complete -F _grimoire_complete grimoire
`
}

// zshCompletion needs compinit to have run first; without compdef it does
// nothing rather than failing the shell's startup.
func zshCompletion(words string) string {
	return `# zsh completion for grimoire: source <(grimoire completion zsh)
_grimoire() {
  local -a commands
  commands=(` + words + `)
  if (( CURRENT == 2 )); then
    compadd -a commands
  fi
}
(( $+functions[compdef] )) && compdef _grimoire grimoire
`
}

// fishCompletion offers the words only where a subcommand is expected.
func fishCompletion(words string) string {
	return `# fish completion for grimoire: grimoire completion fish | source
complete -c grimoire -n __fish_use_subcommand -a "` + words + `"
`
}
