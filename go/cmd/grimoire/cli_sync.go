package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cloudsync"
)

// `grimoire sync folder|status|now|off|deleted|restore` — sync through a
// folder a cloud drive syncs. The peer form, `grimoire sync PEER_URL`, is in
// cli.go and unchanged.

func cloudSyncCommands() map[string]func([]string) int {
	return map[string]func([]string) int{
		"folder": cmdSyncFolder, "status": cmdSyncStatus, "now": cmdSyncNow,
		"off": cmdSyncOff, "deleted": cmdSyncDeleted, "restore": cmdSyncRestore,
	}
}

// syncPassphrase takes the passphrase from the environment, a pipe, or a
// prompt, in that order. confirm asks twice when a new backup is being made.
func syncPassphrase(confirm bool) (string, error) {
	if p, err := syncPassphraseFromEnv(); err != nil || p != "" {
		return p, err
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("no passphrase on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	p, err := readPassword("sync passphrase: ")
	if err != nil || !confirm {
		return p, err
	}
	again, err := readPassword("again, to confirm: ")
	if err != nil {
		return "", err
	}
	if again != p {
		return "", fmt.Errorf("the passphrases do not match")
	}
	return p, nil
}

func cmdSyncFolder(args []string) int {
	rest, flags := parseSecretFlags(args)
	if len(rest) != 1 {
		return fail("usage: grimoire sync folder PATH [--device NAME] [--interval SECONDS]")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	probe := e.cloud.ProbeFolder(rest[0])
	if probe.Problem != nil {
		return fail("%s", probe.Problem.Error())
	}
	if probe.Downloading {
		return fail("Your cloud drive has not finished downloading this folder yet. Try again once it has.")
	}
	create := !probe.HasBackup
	if create {
		fmt.Fprintf(os.Stderr, "No Grimoire backup in %s yet; starting a new one.\n", probe.Path)
		fmt.Fprintln(os.Stderr, "If you forget this passphrase, the backup can't be read. Grimoire can't recover it.")
	} else {
		fmt.Fprintf(os.Stderr, "Found a Grimoire backup in %s.\n", probe.Path)
	}
	pass, err := syncPassphrase(create)
	if err != nil {
		return fail("%v", err)
	}
	interval, _ := strconv.Atoi(flags["interval"])
	if _, err := e.cloud.Setup(cloudsync.SetupOptions{Folder: probe.Path, Passphrase: pass,
		Create: create, DeviceName: flags["device"], Interval: interval}); err != nil {
		return fail("%s", err.Error())
	}
	st, err := e.cloud.SyncOnce()
	if err != nil {
		return fail("set up, but the first sync failed: %s", err.Error())
	}
	fmt.Printf("syncing %s through %s as %q\n", e.vault.Root, probe.Path, e.cloud.DeviceName())
	printSyncStats(st)
	fmt.Println("A running `grimoire serve` keeps it in sync from now on.")
	return 0
}

func printSyncStats(st cloudsync.Stats) {
	fmt.Printf("uploaded %d, downloaded %d, deleted %d, merged %d, conflicts %d",
		st.Uploaded, st.Downloaded, st.Deleted, st.Merged, st.Conflicts)
	if st.Waiting > 0 {
		fmt.Printf(", waiting on %d files your cloud drive has not downloaded yet", st.Waiting)
	}
	fmt.Println()
}

func ago(ms int64) string {
	if ms == 0 {
		return "never"
	}
	d := time.Since(time.UnixMilli(ms)).Round(time.Second)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

func cmdSyncStatus(args []string) int {
	e, err := newEnv(false)
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	st := e.cloud.Status()
	if !st.Enabled {
		fmt.Println("folder sync: off")
		if e.server.SyncPeer != "" {
			fmt.Printf("peer sync: %s\n", e.server.SyncPeer)
		}
		return 0
	}
	fmt.Printf("folder sync: %s\n", st.Folder)
	fmt.Printf("this device: %s, every %ds\n", st.DeviceName, st.Interval)
	fmt.Printf("last synced: %s\n", ago(st.LastSync))
	if st.Error != nil {
		fmt.Printf("problem: %s\n", st.Error.Msg)
	} else if st.Stats.Waiting > 0 {
		fmt.Printf("waiting on %d files your cloud drive has not downloaded yet\n", st.Stats.Waiting)
	}
	fmt.Printf("devices: %d\n", len(st.Devices))
	for _, d := range st.Devices {
		tag := ""
		if d.Self {
			tag = " (this device)"
		}
		issue := ""
		if d.Issue != "" {
			issue = " · " + strings.ReplaceAll(d.Issue, "_", " ")
		}
		fmt.Printf("  %s%s · last seen %s%s\n", d.Name, tag, ago(d.LastSeen), issue)
	}
	if st.Error != nil {
		return 1
	}
	return 0
}

func cmdSyncNow(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	st, err := e.cloud.SyncOnce()
	if err != nil {
		return fail("%s", err.Error())
	}
	printSyncStats(st)
	return 0
}

func cmdSyncOff(args []string) int {
	e, err := newEnv(false)
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if err := e.cloud.Disable(); err != nil {
		return fail("%v", err)
	}
	fmt.Println("folder sync is off on this device; the backup in the folder was left as it is")
	return 0
}

func cmdSyncDeleted(args []string) int {
	e, err := newEnv(false)
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	del, err := e.cloud.Deleted()
	if err != nil {
		return fail("%s", err.Error())
	}
	if len(del) == 0 {
		fmt.Println("nothing deleted in the last 90 days that the folder can restore")
		return 0
	}
	for _, d := range del {
		fmt.Printf("%s\tdeleted %s on %s\n", d.Path, ago(d.DeletedAt), d.Device)
	}
	return 0
}

func cmdSyncRestore(args []string) int {
	if len(args) != 1 {
		return fail("usage: grimoire sync restore PATH")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if err := e.cloud.Restore(args[0]); err != nil {
		return fail("%s", err.Error())
	}
	if _, err := e.cloud.SyncOnce(); err != nil {
		fmt.Fprintf(os.Stderr, "restored locally; it will reach your other devices on the next sync (%v)\n", err)
	}
	fmt.Printf("restored %s\n", args[0])
	return 0
}
