// Command embedhex prints a text's embedding as hex, for byte-comparison
// against the Python implementation.
package main

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

func main() {
	m, err := embed.LoadModel2Vec(os.Getenv("GRIMOIRE_MODEL_DIR"), "minishlab/potion-base-8M")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	v := m.Embed([]string{os.Args[1]})[0]
	fmt.Println(hex.EncodeToString(index.Pack(v)))
}
