// Command sign creates and uses Ed25519 signing keys in a
// minisign-compatible format, for environments without the minisign
// binary and for tests. Its signatures and public-key files are byte
// compatible with real minisign output ("Ed" algorithm); its SECRET key
// file is a localmesh-only format — real minisign cannot load it, and
// maintainers should prefer `minisign -G`/`minisign -S` when available.
//
// Usage:
//
//	sign keygen -sec KEY.sec -pub KEY.pub
//	sign pubkey -sec KEY.sec          # re-emit public key file
//	sign -sec KEY.sec -m FILE         # writes FILE.minisig
package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/localmesh/localmesh/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "pubkey":
		err = cmdPubkey(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = cmdSign(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `sign — localmesh release signing helper (minisign-compatible output)

  sign keygen -sec KEY.sec -pub KEY.pub   generate a signing keypair
  sign pubkey -sec KEY.sec                print the public key file
  sign -sec KEY.sec -m FILE               write FILE.minisig

The .pub and .minisig files interoperate with real minisign ("Ed"
signatures). The .sec file does NOT — it is unencrypted raw key
material; keep it like a password.
`)
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	sec := fs.String("sec", "", "secret key output path")
	pub := fs.String("pub", "", "public key output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sec == "" || *pub == "" {
		return fmt.Errorf("keygen requires -sec and -pub")
	}
	priv, k, pubData, err := release.GenerateMinisignKeyPair()
	if err != nil {
		return err
	}
	if err := os.WriteFile(*sec, release.MarshalSigningSecretKey(k.KeyNum, priv), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(*pub, pubData, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sign: wrote %s (keep secret) and %s (distribute)\n  key id: %s\n", *sec, *pub, k.KeyIDHex)
	return nil
}

func cmdPubkey(args []string) error {
	fs := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	sec := fs.String("sec", "", "secret key path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sec == "" {
		return fmt.Errorf("pubkey requires -sec")
	}
	keynum, priv, err := readSecret(*sec)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	fmt.Print(string(release.MarshalMinisignPublicKey(keynum, pub)))
	return nil
}

func cmdSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	sec := fs.String("sec", "", "secret key path")
	msg := fs.String("m", "", "file to sign")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sec == "" || *msg == "" {
		return fmt.Errorf("sign requires -sec and -m")
	}
	keynum, priv, err := readSecret(*sec)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*msg)
	if err != nil {
		return err
	}
	comment := fmt.Sprintf("timestamp:%d\tlocalmesh release", time.Now().Unix())
	out := release.SignMinisign(priv, keynum, data, comment)
	if err := os.WriteFile(*msg+release.MinisignSigExt, out, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sign: wrote %s (key id %s)\n", *msg+release.MinisignSigExt, release.MinisignKeyID(keynum))
	return nil
}

func readSecret(path string) ([8]byte, ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [8]byte{}, nil, err
	}
	keynum, priv, err := release.ParseSigningSecretKey(data)
	if err != nil {
		return [8]byte{}, nil, fmt.Errorf("%s is not a localmesh signing key — for a real minisign secret key use the minisign binary to sign", path)
	}
	return keynum, priv, nil
}
