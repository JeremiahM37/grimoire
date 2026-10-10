package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// memoryImageCmd stores a picture as an image memory. The file is read here and
// sent as base64, so the same command works against a remote server and the
// in-process one. Without --caption the picture is stored but is not searchable.
func memoryImageCmd(e *env, args []string) int {
	pos := positional(args)
	if len(pos) == 0 {
		return fail("usage: grimoire memory image FILE [--caption TEXT] [--topic T]")
	}
	raw, err := os.ReadFile(pos[0])
	if err != nil {
		return fail("memory image: %v", err)
	}
	body := map[string]any{
		"data_base64": base64.StdEncoding.EncodeToString(raw),
		"caption":     flagStr(args, "--caption"),
		"topic":       flagStr(args, "--topic"),
	}
	status, out := e.callBody(http.MethodPost, "/api/memory/image", body)
	if status != http.StatusCreated {
		return fail("memory image failed: %s", out)
	}
	var res struct {
		Path         string `json:"path"`
		Text         string `json:"text"`
		CaptionBasis string `json:"caption_basis"`
		Searchable   bool   `json:"searchable"`
		Image        struct {
			SHA   string `json:"sha"`
			MIME  string `json:"mime"`
			Bytes int64  `json:"bytes"`
		} `json:"image"`
		Stripped struct {
			EXIF bool `json:"exif"`
		} `json:"stripped"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return fail("memory image: unreadable answer: %v", err)
	}
	fmt.Printf("stored %s (%s, %d bytes) in %s\n", res.Image.SHA, res.Image.MIME, res.Image.Bytes, res.Path)
	if res.Stripped.EXIF {
		fmt.Println("  EXIF metadata removed")
	}
	if res.Searchable {
		fmt.Printf("  caption (%s): %s\n", res.CaptionBasis, res.Text)
	} else {
		fmt.Println("  no caption given: the picture is stored but NOT searchable")
	}
	return 0
}

func flagStr(args []string, name string) string {
	v, _ := flagValue(args, name)
	return strings.TrimSpace(v)
}
