package main

import (
	"fmt"
	"golang.org/x/mod/sumdb/dirhash"
	"log"
	"os"
)

func main() {
	hash, err := dirhash.HashZip(os.Args[1], dirhash.Hash1)
	if err != nil {
		log.Fatalf(fmt.Sprintf("failed to HashZip: %s", err.Error()))
	}
	log.Printf("Hash: %s", hash)
}
