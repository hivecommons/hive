package main

import (
	"crypto/rsa"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/crux/forgejwt"
)

// forgeSubcommand holds operator commands for a self-hosted forge:
//
//	hive forge key-list --issuer URL --key FILE [--key FILE] [--out DIR]
//
// key-list writes the discovery document and key list that Forgejo fetches to
// verify the JWTs hive signs. Serve DIR at the issuer URL. Pass --key once per
// signing key; during a rotation pass the old and the new key.
const forgeSubcommand = "forge"

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runForge(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "key-list" {
		fmt.Fprintln(stderr, "usage: hive forge key-list --issuer URL --key FILE [--key FILE] [--out DIR]")
		return 2
	}
	fs := flag.NewFlagSet("forge key-list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	issuer := fs.String("issuer", "", "issuer URL, https://host[:port]; Forgejo's integration --issuer")
	out := fs.String("out", ".", "directory to write "+forgejwt.DiscoveryPath+" and "+forgejwt.KeyListPath+" into")
	var keys stringList
	fs.Var(&keys, "key", "signing key file (PEM RSA private key, mode 0600, or 0440 with group hive-launch); repeat for each key to publish")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || len(keys) == 0 {
		fs.Usage()
		return 2
	}
	iss, err := forgejwt.ParseIssuer(*issuer)
	if err != nil {
		fmt.Fprintf(stderr, "forge key-list: %v\n", err)
		return 2
	}
	var pubs []*rsa.PublicKey
	for _, k := range keys {
		pub, err := forgejwt.PublicKey(k)
		if err != nil {
			fmt.Fprintf(stderr, "forge key-list: %v\n", err)
			return 1
		}
		pubs = append(pubs, pub)
	}
	discovery, jwks, err := forgejwt.KeyList(iss, pubs...)
	if err != nil {
		fmt.Fprintf(stderr, "forge key-list: %v\n", err)
		return 1
	}
	for _, f := range []struct {
		path string
		data []byte
	}{{forgejwt.DiscoveryPath, discovery}, {forgejwt.KeyListPath, jwks}} {
		p := filepath.Join(*out, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintf(stderr, "forge key-list: %v\n", err)
			return 1
		}
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			fmt.Fprintf(stderr, "forge key-list: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %s\n", p)
	}
	return 0
}
