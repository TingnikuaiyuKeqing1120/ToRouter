// Run with go run ./scripts/release-keygen to generate an Ed25519 release signing keypair.
// The private key must remain in a secure, git-ignored directory (.local/ or release environment).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	outDir := flag.String("dir", ".local/release", "directory to store keypair (git-ignored)")
	force := flag.Bool("force", false, "overwrite existing key files if present")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create directory: %v\n", err)
		os.Exit(1)
	}

	privPath := filepath.Join(*outDir, "update-private.key")
	pubPath := filepath.Join(*outDir, "update-public.key")

	if !*force {
		if _, err := os.Stat(privPath); err == nil {
			fmt.Fprintf(os.Stderr, "error: %s already exists. Use -force to overwrite.\n", privPath)
			os.Exit(1)
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to generate ed25519 key: %v\n", err)
		os.Exit(1)
	}

	privB64 := base64.StdEncoding.EncodeToString(priv)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	if err := os.WriteFile(privPath, []byte(privB64+"\n"), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write private key: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(pubPath, []byte(pubB64+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write public key: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("==> Generated Ed25519 release keypair:\n")
	fmt.Printf("  Private key: %s (KEEP SECRET, NEVER COMMIT TO GIT)\n", privPath)
	fmt.Printf("  Public key:  %s\n", pubPath)
	fmt.Printf("\nBase64 Public Key (inject into build):\n%s\n", pubB64)
}
