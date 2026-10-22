// Command agekey prints a fresh age key pair as environment assignments for
// test stands that have no age CLI.
package main

import (
	"fmt"
	"os"

	"filippo.io/age"
)

func main() {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("AGE_RECIPIENTS=%s\nAGE_IDENTITY=%s\n", id.Recipient(), id)
}
