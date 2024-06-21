package main

import (
	"fmt"
	"golang.org/x/mod/sumdb/dirhash"
	"log"
	"os"
)

func main() {
	hash, err := dirhash.HashDir(os.Args[1], os.Args[2], dirhash.Hash1)
	if err != nil {
		log.Fatalf(fmt.Sprintf("failed to HashDir: %s", err.Error()))
	}
	log.Printf("Hash: %s", hash)
}
