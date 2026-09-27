// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command stamp prints a digest of the text it embeds.
package main

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
)

//go:embed input.txt
var input []byte

func main() {
	fmt.Printf("stamp %x\n", sha256.Sum256(input))
}
