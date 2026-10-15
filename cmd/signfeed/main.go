// Command signfeed signs an HTML Clay update feed payload, or creates a signing keypair.
//
//	signfeed keygen                       prints a new seed and public key, both base64
//	signfeed sign -key-id ID payload.json  reads the seed from HTMLCLAY_UPDATE_SEED, prints the envelope
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"

	"github.com/panphora/htmlclay/internal/update"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err)
		}
		fmt.Printf("seed:   %s\n", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Printf("public: %s\n", base64.StdEncoding.EncodeToString(pub))
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ExitOnError)
		keyID := fs.String("key-id", "", "id of the signing key, as compiled into the app")
		fs.Parse(os.Args[2:])
		if *keyID == "" || fs.NArg() != 1 {
			usage()
		}
		seed, err := base64.StdEncoding.DecodeString(os.Getenv("HTMLCLAY_UPDATE_SEED"))
		if err != nil || len(seed) != ed25519.SeedSize {
			fail(fmt.Errorf("HTMLCLAY_UPDATE_SEED must be a base64 %d-byte seed", ed25519.SeedSize))
		}
		payload, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		env, err := update.SignPayload(payload, *keyID, ed25519.NewKeyFromSeed(seed))
		if err != nil {
			fail(err)
		}
		pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		if _, err := update.VerifyFeed(env, map[string]ed25519.PublicKey{*keyID: pub}); err != nil {
			fail(fmt.Errorf("signed feed does not verify: %w", err))
		}
		os.Stdout.Write(append(env, '\n'))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: signfeed keygen | signfeed sign -key-id ID payload.json")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "signfeed:", err)
	os.Exit(1)
}
