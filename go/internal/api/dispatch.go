package api

import (
	"net/http"
	"strings"
)

// Note paths contain slashes ("Job Search/Strategy.md"), and Go's ServeMux
// requires a {path...} wildcard to be the LAST element of a pattern — so
// /api/notes/{path...}/pin cannot be registered the way FastAPI's
// /notes/{path:path}/pin can. Instead one wildcard route per method captures
// everything and dispatches on the trailing action segment here.
//
// The discriminator is the .md suffix: normPath gives every real note path one,
// and no action segment has it. So "notes/pin.md" is a note and
// "notes/foo.md/pin" is the pin action on foo.md, with no ambiguity.

// noteActions are the trailing segments that mean "do this to the note" rather
// than "this is part of the note path".
var noteActions = map[string]bool{
	"pin": true, "rename": true, "duplicate": true, "link": true,
	"unlinked": true, "encrypt": true, "decrypt": true, "history": true,
	"export.html": true,
}

// splitNoteAction separates a wildcard path into the note path, an action, and
// any action arguments.
func splitNoteAction(raw string) (notePath, action string, args []string) {
	parts := strings.Split(strings.Trim(raw, "/"), "/")

	// walk back from the end while the segments look like an action + args
	for i := len(parts) - 1; i > 0; i-- {
		if !noteActions[parts[i]] {
			continue
		}
		// everything before i is the note path; everything after is arguments
		return strings.Join(parts[:i], "/"), parts[i], parts[i+1:]
	}
	return raw, "", nil
}

func (s *Server) noteGet(w http.ResponseWriter, r *http.Request) {
	notePath, action, args := splitNoteAction(r.PathValue("path"))
	// Every read of a single note comes through here — the note itself, its
	// history, its unlinked mentions, its HTML export — so the space check
	// belongs here rather than in each of them.
	if !s.requireRead(w, r, normPath(notePath)) {
		return
	}
	switch action {
	case "":
		s.getNote(w, r)
	case "unlinked":
		s.withNotePath(notePath, s.unlinkedMentions)(w, r)
	case "history":
		switch len(args) {
		case 0:
			s.withNotePath(notePath, s.noteHistory)(w, r)
		case 1:
			s.withNotePathAndVersion(notePath, args[0], s.noteHistoryVersion)(w, r)
		default:
			writeErr(w, http.StatusNotFound, "no such route")
		}
	case "export.html":
		s.withNotePath(notePath, s.exportNote)(w, r)
	default:
		writeErr(w, http.StatusNotFound, "no such route")
	}
}

func (s *Server) notePost(w http.ResponseWriter, r *http.Request) {
	notePath, action, args := splitNoteAction(r.PathValue("path"))
	if !s.requireWrite(w, r, normPath(notePath)) {
		return
	}
	switch action {
	case "pin":
		s.withNotePath(notePath, s.togglePin)(w, r)
	case "rename":
		s.withNotePath(notePath, s.renameNote)(w, r)
	case "duplicate":
		s.withNotePath(notePath, s.duplicateNote)(w, r)
	case "link":
		s.withNotePath(notePath, s.linkNote)(w, r)
	case "encrypt":
		s.withNotePath(notePath, s.encryptNote)(w, r)
	case "decrypt":
		s.withNotePath(notePath, s.decryptNote)(w, r)
	case "history":
		if len(args) == 2 && args[1] == "restore" {
			s.withNotePathAndVersion(notePath, args[0], s.restoreVersion)(w, r)
			return
		}
		writeErr(w, http.StatusNotFound, "no such route")
	default:
		writeErr(w, http.StatusNotFound, "no such route")
	}
}

// withNotePath rewrites the request's path value so the target handler sees the
// note path alone, without the action suffix.
func (s *Server) withNotePath(notePath string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("path", notePath)
		h(w, r)
	}
}

func (s *Server) withNotePathAndVersion(notePath, version string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("path", notePath)
		r.SetPathValue("version", version)
		h(w, r)
	}
}
