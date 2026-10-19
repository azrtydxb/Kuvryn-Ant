package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
)

const recoveryUsage = `usage: ant federation recovery sign-rotation --template FILE --key FILE --new-key-out FILE
       ant federation recovery sign-authority-cert --template FILE --key FILE [--rotation FILE]
       ant federation recovery sign-challenge --challenge FILE --key FILE
FILE "-" for --template, --rotation or --challenge reads standard input. All commands run offline (no database, no network).`

// federationRecoveryCommand signs recovery-key rotations offline, so the
// recovery private key never reaches a server (§5.1, D2):
//
//  1. GET /api/v1/federation/recovery-key/rotation on the authority -> template.json
//  2. ant federation recovery sign-rotation --template template.json --key current.key --new-key-out new.key > rotation.json
//  3. ant federation recovery sign-authority-cert --template template.json --key new.key --rotation rotation.json > request.json
//  4. POST request.json to /api/v1/federation/recovery-key/rotate
//
// Forced authority recovery (the authority is lost or compromised) uses the same
// signer on the member that takes over:
//
//  1. POST /api/v1/federation/authority/recovery-template on that member -> template.json
//  2. ant federation recovery sign-authority-cert --template template.json --key recovery.key > request.json
//  3. POST request.json (plus "confirm": true) to /api/v1/federation/authority/recover
//
// The recovery-key drill proves possession without changing anything:
//
//  1. POST /api/v1/federation/recovery-key/challenge on the authority -> challenge.json
//  2. ant federation recovery sign-challenge --challenge challenge.json --key recovery.key > proof.json
//  3. POST proof.json to /api/v1/federation/recovery-key/verify
//
// Key files hold the recovery key export (ant-recovery-v1:…); keys are never
// accepted on the command line.
func federationRecoveryCommand(logger *slog.Logger, args []string, stdin io.Reader, output io.Writer) int {
	if len(args) == 0 || (args[0] != "sign-rotation" && args[0] != "sign-authority-cert" && args[0] != "sign-challenge") {
		logger.Error(recoveryUsage)
		return 1
	}
	if args[0] == "sign-challenge" {
		return recoverySignChallenge(logger, args[1:], stdin, output)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	templatePath := fs.String("template", "", "rotation template (JSON from GET /api/v1/federation/recovery-key/rotation)")
	keyPath := fs.String("key", "", "recovery key file")
	newKeyOut := fs.String("new-key-out", "", "file to create for the new recovery key (sign-rotation)")
	rotationPath := fs.String("rotation", "", "output of sign-rotation to merge (sign-authority-cert)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *templatePath == "" || *keyPath == "" ||
		(args[0] == "sign-rotation" && (*newKeyOut == "" || *rotationPath != "")) || (args[0] == "sign-authority-cert" && *newKeyOut != "") ||
		(*templatePath == "-" && *rotationPath == "-") {
		logger.Error(recoveryUsage)
		return 1
	}
	var tpl federation.RecoveryTemplate
	if err := readJSONFile(*templatePath, stdin, &tpl); err != nil {
		logger.Error("rotation template unreadable", "reason", err)
		return 1
	}
	key, err := readKeyFile(*keyPath)
	if err != nil {
		logger.Error("recovery key unreadable", "reason", err)
		return 1
	}
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	switch args[0] {
	case "sign-rotation":
		export, public, err := federation.NewRecoveryKey()
		if err != nil {
			logger.Error("key generation failed", "reason", err)
			return 1
		}
		rot, err := federation.SignRecoveryRotation(tpl, key, public)
		if err != nil {
			logger.Error("rotation not signed", "reason", err)
			return 1
		}
		// The new key is written only after the statement is signed, and
		// never over an existing file.
		f, err := os.OpenFile(*newKeyOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			logger.Error("cannot create the new key file", "reason", err)
			return 1
		}
		_, werr := fmt.Fprintln(f, export)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(*newKeyOut)
			logger.Error("cannot write the new key file", "reason", werr)
			return 1
		}
		_ = enc.Encode(map[string]any{"rotation": rot})
		logger.Info("rotation signed; store the new key offline and sign the authority certificate with it", "new_key_file", *newKeyOut,
			"new_key_fingerprint", federation.KeyFingerprint(public), "generation", rot.Generation)
	case "sign-authority-cert":
		if *rotationPath == "" {
			// A forced recovery is certified by the current recovery key; refuse a
			// wrong key here instead of producing a request the server rejects.
			if pub, err := federation.RecoveryPublicKey(key); err != nil || pub != tpl.PreviousKey {
				logger.Error("the key is not the current recovery key named by the template")
				return 1
			}
		}
		cert, err := federation.SignAuthorityCertificate(tpl, key)
		if err != nil {
			logger.Error("authority certificate not signed", "reason", err)
			return 1
		}
		out := map[string]any{"authority_certificate": cert}
		if *rotationPath != "" {
			var prior struct {
				Rotation *federation.RecoveryRotation `json:"rotation"`
			}
			if err := readJSONFile(*rotationPath, stdin, &prior); err != nil || prior.Rotation == nil {
				logger.Error("rotation file unreadable", "reason", err)
				return 1
			}
			if pub, err := federation.RecoveryPublicKey(key); err != nil || prior.Rotation.NewKey != pub || prior.Rotation.Generation != tpl.Generation {
				logger.Error("the rotation does not name this key as the new recovery key for the template generation")
				return 1
			}
			out["rotation"] = prior.Rotation
		}
		_ = enc.Encode(out)
	}
	return 0
}

func readJSONFile(path string, stdin io.Reader, v any) error {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func readKeyFile(path string) (string, error) {
	if path == "-" {
		return "", errors.New("read the key from a file, not standard input")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// recoverySignChallenge signs a recovery-key drill challenge offline.
func recoverySignChallenge(logger *slog.Logger, args []string, stdin io.Reader, output io.Writer) int {
	fs := flag.NewFlagSet("sign-challenge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	challengePath := fs.String("challenge", "", "challenge (JSON from POST /api/v1/federation/recovery-key/challenge)")
	keyPath := fs.String("key", "", "recovery key file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *challengePath == "" || *keyPath == "" {
		logger.Error(recoveryUsage)
		return 1
	}
	var c federation.RecoveryChallenge
	if err := readJSONFile(*challengePath, stdin, &c); err != nil {
		logger.Error("challenge unreadable", "reason", err)
		return 1
	}
	key, err := readKeyFile(*keyPath)
	if err != nil {
		logger.Error("recovery key unreadable", "reason", err)
		return 1
	}
	sig, err := federation.SignRecoveryChallenge(c, key)
	if err != nil {
		logger.Error("challenge not signed", "reason", err)
		return 1
	}
	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{"challenge": c.Challenge, "signature": sig})
	return 0
}
