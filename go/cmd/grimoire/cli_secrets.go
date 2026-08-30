package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// Managing credentials from the shell.
//
// The broker is the agent path and stays the point of this vault: an agent
// gets a grant and the server makes the call, so the value never reaches it.
// But an operator has to be able to run the vault — add, rotate, roll back,
// see what is about to expire — and doing that only through a browser makes
// the credential store the one part of Grimoire that cannot be scripted.
//
// `run` is the deliberate exception, and it is worth naming as one: it hands
// values to a child process, which is exactly what the broker exists to avoid.
// It is for the case the broker cannot serve — your own build, your own shell,
// a program that wants an environment variable — and the help says so, because
// a security model people work around silently is worse than one with a
// documented door.

func cmdSecret(args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire secret init|list|add|rm|history|restore|check|scan|import|export …")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	v := e.server.Secrets

	// init is the one subcommand that runs against a vault that does not exist
	// yet, so it comes before the unlock. Without it the credential store was
	// the one part of Grimoire that could only be created from a browser,
	// which a headless install does not have.
	if args[0] == "init" {
		if v.IsInitialized() {
			return fail("this vault already exists; `grimoire secret list` to see it")
		}
		pass, err := passphraseForInit()
		if err != nil {
			return fail("%v", err)
		}
		if err := v.Initialize(pass); err != nil {
			return fail("%v", err)
		}
		fmt.Println("credential vault created. It is unlocked now and seals when this exits;")
		fmt.Println("set GRIMOIRE_VAULT_PASSPHRASE_FILE for a service that must unlock itself.")
		return 0
	}

	// scan reads NOTES, not the vault, so it runs without unlocking anything —
	// and it is most useful exactly when nothing is stored yet, which is when
	// keys are likeliest to be sitting in notes instead.
	if args[0] == "scan" {
		// Reads notes, not the vault, so it is useful even before anything is
		// stored — which is exactly when a vault is most likely to have keys
		// sitting in notes instead.
		paths, err := e.vault.Walk()
		if err != nil {
			return fail("%v", err)
		}
		var found []secrets.Finding
		for _, p := range paths {
			note, err := e.vault.Read(p)
			if err != nil {
				continue
			}
			found = append(found, secrets.ScanText(p, note.Raw)...)
		}
		if len(found) == 0 {
			fmt.Printf("scanned %d notes; no credentials found in them\n", len(paths))
			return 0
		}
		for _, f := range found {
			fmt.Printf("%-9s %s:%d  %s  %s\n", f.Confidence, f.Path, f.Line, f.Kind, f.Masked)
		}
		fmt.Printf("\n%d finding(s) across %d notes. Values are masked on purpose.\n",
			len(found), len(paths))
		fmt.Println("Rotate anything real at the issuer first — a key that has been in a")
		fmt.Println("synced note should be treated as disclosed — then store it and edit the note.")
		return 1
	}

	if err := unlockForCLI(v); err != nil {
		return fail("%v", err)
	}

	switch args[0] {
	case "list", "ls":
		info, err := v.Describe()
		if err != nil {
			return fail("%v", err)
		}
		if len(info) == 0 {
			fmt.Println("no secrets stored")
			return 0
		}
		for _, i := range info {
			mark := map[string]string{
				secrets.StatusExpired:  "EXPIRED",
				secrets.StatusExpiring: "expires soon",
				secrets.StatusStale:    "due for rotation",
			}[i.Status]
			extra := []string{}
			if i.Versions > 0 {
				extra = append(extra, fmt.Sprintf("%d prior", i.Versions))
			}
			if i.Uses > 0 {
				extra = append(extra, fmt.Sprintf("%d uses", i.Uses))
			} else {
				extra = append(extra, "never used")
			}
			if mark != "" {
				extra = append(extra, mark)
			}
			fmt.Printf("%-32s %s\n", i.Name, strings.Join(extra, " · "))
		}
		return 0

	case "add", "set":
		rest, flags := parseSecretFlags(args[1:])
		if len(rest) == 0 {
			return fail("usage: grimoire secret add NAME [--note TEXT] [--expires YYYY-MM-DD] [--rotate-days N]")
		}
		name := rest[0]
		value, err := readPassword("value for " + name + ": ")
		if err != nil {
			return fail("%v", err)
		}
		if value == "" {
			return fail("empty value; nothing stored")
		}
		meta := map[string]any{}
		if s, ok := flags["note"]; ok {
			meta[secrets.MetaNote] = s
		}
		if s, ok := flags["expires"]; ok {
			meta[secrets.MetaExpires] = s
		}
		if s, ok := flags["rotate-days"]; ok {
			n, err := strconv.Atoi(s)
			if err != nil {
				return fail("--rotate-days wants a number, got %q", s)
			}
			meta[secrets.MetaRotateDays] = n
		}
		if err := v.PutVersioned(name, value, meta, flags["reason"]); err != nil {
			return fail("%v", err)
		}
		vers, _ := v.Versions(name)
		if len(vers) > 0 {
			fmt.Printf("stored %s — the previous value is retained; "+
				"`grimoire secret restore %s` puts it back\n", name, name)
		} else {
			fmt.Printf("stored %s\n", name)
		}
		return 0

	case "rm", "delete":
		if len(args) < 2 {
			return fail("usage: grimoire secret rm NAME")
		}
		if err := v.Delete(args[1]); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("deleted %s (history goes with it)\n", args[1])
		return 0

	case "history":
		if len(args) < 2 {
			return fail("usage: grimoire secret history NAME")
		}
		vers, err := v.Versions(args[1])
		if err != nil {
			return fail("%v", err)
		}
		if len(vers) == 0 {
			fmt.Println("no previous versions")
			return 0
		}
		for i, ver := range vers {
			fmt.Printf("%d  replaced %s  %s\n", i, ver.At, ver.Note)
		}
		fmt.Printf("\n`grimoire secret restore %s [N]` makes one of these current again.\n", args[1])
		return 0

	case "restore":
		if len(args) < 2 {
			return fail("usage: grimoire secret restore NAME [VERSION]")
		}
		idx := 0
		if len(args) > 2 {
			n, err := strconv.Atoi(args[2])
			if err != nil {
				return fail("version must be a number, got %q", args[2])
			}
			idx = n
		}
		if err := v.Restore(args[1], idx); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("restored %s to version %d — the value it replaced is retained too\n", args[1], idx)
		return 0

	case "check":
		// Exit non-zero when something needs doing, so this works from cron
		// or a healthcheck without anybody parsing the output.
		need, err := v.NeedsAttention()
		if err != nil {
			return fail("%v", err)
		}
		if len(need) == 0 {
			fmt.Println("all credentials are current")
			return 0
		}
		for _, i := range need {
			switch i.Status {
			case secrets.StatusExpired:
				fmt.Printf("EXPIRED       %-28s expired %s\n", i.Name, i.Expires)
			case secrets.StatusExpiring:
				fmt.Printf("expiring      %-28s in %d day(s), %s\n", i.Name, *i.ExpiresInDays, i.Expires)
			case secrets.StatusStale:
				fmt.Printf("rotation due  %-28s last changed %s\n", i.Name, i.Updated)
			}
		}
		return 1

	case "import":
		if len(args) < 2 {
			return fail("usage: grimoire secret import FILE.env")
		}
		return importDotenv(v, args[1])

	case "export":
		_, flags := parseSecretFlags(args[1:])
		if _, ok := flags["reveal"]; !ok {
			return fail("export writes every VALUE in cleartext to stdout.\n" +
				"That is a real thing to want — a backup, a migration — and a\n" +
				"terrible thing to do by accident, so it needs --reveal.")
		}
		return exportDotenv(v)
	}
	return fail("unknown: grimoire secret %s", args[0])
}

// unlockForCLI opens the vault, preferring the passphrase file a service
// already uses so scripts do not have to prompt.
func unlockForCLI(v *secrets.Vault) error {
	if !v.IsInitialized() {
		return fmt.Errorf("no credential vault here yet — create one in the console, " +
			"or set GRIMOIRE_VAULT_PASSPHRASE_FILE")
	}
	if v.IsUnlocked() {
		return nil
	}
	if path := strings.TrimSpace(os.Getenv("GRIMOIRE_VAULT_PASSPHRASE_FILE")); path != "" {
		if err := v.UnlockFromFile(path); err == nil {
			return nil
		}
	}
	pass, err := readPassword("vault passphrase: ")
	if err != nil {
		return err
	}
	return v.Unlock(pass)
}

// booleanFlags never take a value.
//
// Declared rather than inferred, because "the next word unless it starts with
// a dash" cannot tell `--all` from `--note`: it makes `--all DEMO` swallow the
// secret name and leave the caller with an empty list and no error.
var booleanFlags = map[string]bool{"all": true, "reveal": true, "force": true}

// parseSecretFlags pulls --key value and --flag out of an argument list.
func parseSecretFlags(args []string) (rest []string, flags map[string]string) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			rest = append(rest, a)
			continue
		}
		name := strings.TrimPrefix(a, "--")
		if k, val, ok := strings.Cut(name, "="); ok {
			flags[k] = val
			continue
		}
		if booleanFlags[name] {
			flags[name] = "true"
			continue
		}
		// A bare flag followed by another flag is a boolean, not a key wanting
		// the next flag as its value.
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			flags[name] = args[i+1]
			i++
			continue
		}
		flags[name] = "true"
	}
	return rest, flags
}

// importDotenv loads KEY=value lines.
func importDotenv(v *secrets.Vault, path string) int {
	f, err := os.Open(path)
	if err != nil {
		return fail("%v", err)
	}
	defer f.Close()
	n, skipped := 0, 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			skipped++
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key == "" || val == "" {
			skipped++
			continue
		}
		if err := v.PutVersioned(key, val, map[string]any{
			secrets.MetaNote: "imported from " + filepath.Base(path),
		}, "imported from "+filepath.Base(path)); err != nil {
			return fail("%v", err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("imported %d secret(s)", n)
	if skipped > 0 {
		fmt.Printf(", skipped %d unparseable line(s)", skipped)
	}
	fmt.Println()
	fmt.Println("Delete the file — it is still cleartext on disk.")
	return 0
}

func exportDotenv(v *secrets.Vault) int {
	info, err := v.Describe()
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintln(os.Stderr, "# every value below is cleartext; redirect with care")
	for _, i := range info {
		val, err := v.Get(i.Name)
		if err != nil {
			continue
		}
		fmt.Printf("%s=%s\n", envName(i.Name), shellQuote(val))
	}
	return 0
}

// cmdRun executes a command with secrets in its environment.
func cmdRun(args []string) int {
	names, cmdArgs := splitAtDoubleDash(args)
	if len(cmdArgs) == 0 {
		return fail("usage: grimoire run NAME[,NAME…] -- command [args…]\n" +
			"       grimoire run --all -- command [args…]\n\n" +
			"Puts credentials in the child's environment. This hands over the\n" +
			"VALUES, which is what the broker exists to avoid — use it for your\n" +
			"own commands, and let agents use grants instead.")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	v := e.server.Secrets
	if err := unlockForCLI(v); err != nil {
		return fail("%v", err)
	}

	rest, flags := parseSecretFlags(names)
	var want []string
	if _, all := flags["all"]; all {
		info, err := v.Describe()
		if err != nil {
			return fail("%v", err)
		}
		for _, i := range info {
			want = append(want, i.Name)
		}
	} else {
		for _, a := range rest {
			for _, n := range strings.Split(a, ",") {
				if n = strings.TrimSpace(n); n != "" {
					want = append(want, n)
				}
			}
		}
	}
	if len(want) == 0 {
		return fail("name at least one secret, or pass --all")
	}
	sort.Strings(want)

	env := os.Environ()
	for _, name := range want {
		// Support NAME=ENVVAR so a secret does not have to be named after the
		// variable the program expects.
		secretName, varName, renamed := strings.Cut(name, "=")
		if !renamed {
			varName = envName(secretName)
		}
		val, err := v.Get(secretName)
		if err != nil {
			return fail("%v", err)
		}
		env = append(env, varName+"="+val)
	}

	bin, err := exec.LookPath(cmdArgs[0])
	if err != nil {
		return fail("%v", err)
	}
	// Exec rather than fork: the child REPLACES this process, so there is no
	// parent holding the values in memory for the child's lifetime, signals and
	// exit codes pass through untouched, and nothing has to be proxied.
	if err := syscall.Exec(bin, cmdArgs, env); err != nil {
		return fail("%v", err)
	}
	return 0
}

// splitAtDoubleDash separates our arguments from the command's.
func splitAtDoubleDash(args []string) (before, after []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// envName turns a secret name into a conventional environment variable.
func envName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// shellQuote makes a value safe to paste back into a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// passphraseForInit reads the passphrase file if one is configured, so a
// headless install can create its vault from configuration rather than from a
// prompt nobody is there to answer.
func passphraseForInit() (string, error) {
	if path := strings.TrimSpace(os.Getenv("GRIMOIRE_VAULT_PASSPHRASE_FILE")); path != "" {
		if pass, err := secrets.ReadPassphraseFile(path); err == nil {
			fmt.Fprintln(os.Stderr, "using the passphrase from "+path)
			return pass, nil
		}
	}
	pass, err := readPassword("new vault passphrase: ")
	if err != nil {
		return "", err
	}
	again, err := readPassword("again: ")
	if err != nil {
		return "", err
	}
	if pass != again {
		return "", fmt.Errorf("passphrases do not match")
	}
	return pass, nil
}
