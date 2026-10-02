package cloudsync

import (
	"errors"
	"fmt"
)

// Error codes. Each has one plain-language message, because the person
// reading it is choosing what to do next, not debugging.
const (
	CodeNotConfigured  = "not_configured"
	CodeFolderMissing  = "folder_missing"
	CodeNotDownloaded  = "not_downloaded"
	CodeWrongPass      = "wrong_passphrase"
	CodeNoBackup       = "no_backup"
	CodeKeyMissing     = "key_missing"
	CodeUnwritable     = "unwritable"
	CodeNewerFormat    = "newer_format"
	CodeVaultInFolder  = "vault_in_folder"
	CodeFolderInVault  = "folder_in_vault"
	CodeBadPassphrase  = "weak_passphrase"
	CodeBusy           = "busy"
	CodeNotFound       = "not_found"
	CodeAlreadyPresent = "exists"
)

var messages = map[string]string{
	CodeNotConfigured:  "Sync to a cloud folder is not set up on this device.",
	CodeFolderMissing:  "The sync folder does not exist. Check that your cloud drive app is installed, signed in and running, and that the folder path is right.",
	CodeNotDownloaded:  "Your cloud drive has not finished downloading the sync folder. Grimoire will try again shortly. On iCloud Drive, turn on \"Keep Downloaded\" for this folder.",
	CodeWrongPass:      "That passphrase does not match the one this backup was created with.",
	CodeNoBackup:       "There is no Grimoire backup in this folder yet.",
	CodeKeyMissing:     "This device no longer has the key for the backup. Enter the passphrase again to reconnect.",
	CodeUnwritable:     "Grimoire cannot write to the sync folder.",
	CodeNewerFormat:    "This backup was made by a newer version of Grimoire. Update Grimoire on this device.",
	CodeVaultInFolder:  "Your notes folder is already inside this folder. Choose a folder outside your notes.",
	CodeFolderInVault:  "The sync folder cannot be inside your notes folder.",
	CodeBadPassphrase:  "Use a passphrase of at least 8 characters.",
	CodeBusy:           "A sync is already running.",
	CodeNotFound:       "That note is not in the backup.",
	CodeAlreadyPresent: "A note with that name already exists on this device.",
}

// Error is a sync failure a person can act on.
type Error struct {
	Code   string `json:"code"`
	Msg    string `json:"message"`
	Detail string `json:"detail,omitempty"`
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Msg + " (" + e.Detail + ")"
	}
	return e.Msg
}

func errf(code, detail string, args ...any) *Error {
	if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	return &Error{Code: code, Msg: messages[code], Detail: detail}
}

// AsError returns err as an *Error, wrapping anything unexpected so callers
// always have a code and a message to show.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: "error", Msg: "Sync failed.", Detail: err.Error()}
}

// IsCode reports whether err carries the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
