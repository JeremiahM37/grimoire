package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/readlog"
)

// cmdAudit prints the read-audit trail.
//
// On the box rather than through the API on purpose: the first question after
// an incident is usually asked by whoever has the shell, and an HTTP-only
// answer needs a working login on the instance being investigated.
func cmdAudit(args []string) int {
	rest, denied := flagOut(args, "--denied")
	path, _ := flagValue(rest, "--path")
	user, _ := flagValue(rest, "--user")
	limit := 50
	if v, ok := flagValue(rest, "--limit"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fail("--limit takes a positive number")
		}
		limit = n
	}

	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	log := readlog.New(e.db)
	rows, err := log.Recent(readlog.Query{Path: path, User: user, Denied: denied, Limit: limit})
	if err != nil {
		return fail("%v", err)
	}
	if len(rows) == 0 {
		if e.auth == nil || !e.auth.Enabled() {
			fmt.Println("no records — this instance is single-user, so no document is restricted")
			return 0
		}
		fmt.Println("no records")
		return 0
	}
	for _, r := range rows {
		who := r.Name
		if who == "" {
			who = "(anonymous)"
		}
		mark := "read"
		if !r.Allowed {
			mark = "DENIED"
		}
		line := fmt.Sprintf("%s  %-6s  %-14s  %s", r.At, mark, who, r.Path)
		if r.Addr != "" {
			line += "  from " + r.Addr
		}
		fmt.Println(strings.TrimRight(line, " "))
	}
	return 0
}
