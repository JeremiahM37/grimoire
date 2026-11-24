package main

import (
	"errors"
	"fmt"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// `grimoire secret keyring on|off|status`: keep the vault passphrase in the
// OS keyring, so the vault opens when its owner logs in. See
// internal/secrets/keyring.go for the sources and their order.
func cmdSecretKeyring(v *secrets.Vault, args []string) int {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "on", "enable":
		if !v.IsInitialized() {
			return fail("no credential vault here yet; `grimoire secret init` first")
		}
		pass, err := readPassword("vault passphrase: ")
		if err != nil {
			return fail("%v", err)
		}
		if err := v.StoreKeyringPassphrase(pass); err != nil {
			return fail("%v", err)
		}
		fmt.Println("The vault passphrase is in the OS keyring. Grimoire unlocks the vault from it")
		fmt.Println("whenever your login keyring is open; GRIMOIRE_VAULT_PASSPHRASE_FILE is no longer needed.")
		return 0
	case "off", "disable":
		if err := v.ForgetKeyringPassphrase(); err != nil {
			return fail("%v", err)
		}
		fmt.Println("Removed the vault passphrase from the OS keyring.")
		return 0
	case "status":
		if p := secrets.SystemdCredentialPath(); p != "" {
			fmt.Println("systemd credential: loaded (" + p + ")")
		} else {
			fmt.Println("systemd credential: not loaded by this process")
		}
		switch _, err := v.KeyringPassphrase(); {
		case err == nil:
			fmt.Println("OS keyring: holds the vault passphrase")
		case errors.Is(err, secrets.ErrNoKeyring):
			fmt.Println("OS keyring: " + err.Error())
		default:
			fmt.Println("OS keyring: no passphrase stored (or the keyring is locked)")
		}
		return 0
	}
	return fail("usage: grimoire secret keyring on|off|status")
}
