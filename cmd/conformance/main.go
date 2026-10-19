// Command conformance runs real pinned ecosystem clients against a Kuvryn Ant instance.
//
//	conformance <target> [formats...]
//
// Environment: ANT_BASE_URL, ANT_TOKEN or ANT_TOKEN_FILE (raw token or credential JSON with "secret"/"token"),
// ANT_CA_BUNDLE (PEM for a private CA), ANT_VERSION (label), CONF_RESULTS_DIR (default conformance/results).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/conformance/harness"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: conformance <target> [formats...]; available:", strings.Join(harness.Available(), " "))
		os.Exit(2)
	}
	if os.Args[1] == "summary" && len(os.Args) == 3 { // conformance summary <results/target/date>: rebuild SUMMARY.md
		if err := harness.WriteSummary(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	base := os.Getenv("ANT_BASE_URL")
	token := os.Getenv("ANT_TOKEN")
	if token == "" && os.Getenv("ANT_TOKEN_FILE") != "" {
		b, err := os.ReadFile(os.Getenv("ANT_TOKEN_FILE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		token = strings.TrimSpace(string(b))
		if strings.HasPrefix(token, "{") {
			var doc map[string]any
			if json.Unmarshal([]byte(token), &doc) == nil {
				token, _ = doc["secret"].(string)
				if token == "" {
					token, _ = doc["token"].(string)
				}
			}
		}
	}
	if base == "" || token == "" {
		fmt.Fprintln(os.Stderr, "ANT_BASE_URL and ANT_TOKEN (or ANT_TOKEN_FILE) are required")
		os.Exit(2)
	}
	ca := os.Getenv("ANT_CA_BUNDLE")
	if ca != "" {
		ca, _ = filepath.Abs(ca)
	}
	ver := os.Getenv("ANT_VERSION")
	if ver == "" {
		out, _ := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
		ver = "git:" + strings.TrimSpace(string(out))
	}
	res := os.Getenv("CONF_RESULTS_DIR")
	if res == "" {
		res = "conformance/results"
	}
	err := harness.Run(harness.Config{Target: os.Args[1], Base: base, Token: token, CA: ca, AntVersion: ver, ResultsDir: res, Formats: os.Args[2:]})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
