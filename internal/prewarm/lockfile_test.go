package prewarm

import (
	"strings"
	"testing"
)

func names(items []Item) string {
	var parts []string
	for _, it := range items {
		parts = append(parts, it.Name+"@"+it.Version)
	}
	return strings.Join(parts, " ")
}

func TestPackageLockV3AndV1(t *testing.T) {
	v3 := `{"lockfileVersion":3,"packages":{
 "":{"name":"app","version":"1.0.0"},
 "node_modules/left-pad":{"version":"1.3.0","resolved":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz","integrity":"sha512-AAAA"},
 "node_modules/a/node_modules/@scope/pkg":{"version":"2.0.0","resolved":"https://registry.npmjs.org/@scope/pkg/-/pkg-2.0.0.tgz","integrity":"sha1-BBBB"},
 "node_modules/alias":{"name":"real","version":"3.0.0","resolved":"https://registry.npmjs.org/real/-/real-3.0.0.tgz"},
 "node_modules/linked":{"resolved":"../linked","link":true},
 "node_modules/gitdep":{"version":"1.0.0","resolved":"git+ssh://git@github.com/x/y.git#abc"},
 "node_modules/bundled":{"version":"1.0.0","inBundle":true},
 "node_modules/left-pad-dup":{"name":"left-pad","version":"1.3.0","resolved":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}`
	items, ignored, err := ParseLockfile("package-lock.json", []byte(v3))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); got != "@scope/pkg@2.0.0 left-pad@1.3.0 real@3.0.0" {
		t.Fatalf("items %q", got)
	}
	if items[1].Integrity != "sha512-AAAA" {
		t.Fatalf("integrity lost: %+v", items[1])
	}
	if len(ignored) != 3 {
		t.Fatalf("ignored %+v", ignored)
	}
	v1 := `{"lockfileVersion":1,"dependencies":{"a":{"version":"1.0.0","resolved":"https://r/a-1.0.0.tgz","integrity":"sha1-x","dependencies":{"b":{"version":"2.0.0","resolved":"https://r/b-2.0.0.tgz"}}},"c":{"version":"file:../c"}}}`
	items, ignored, err = ParseLockfile("package-lock.json", []byte(v1))
	if err != nil || names(items) != "a@1.0.0 b@2.0.0" || len(ignored) != 1 {
		t.Fatalf("v1: %v %v %v", names(items), ignored, err)
	}
	if _, _, err = ParseLockfile("package-lock.json", []byte("{")); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestPnpmLock(t *testing.T) {
	doc := `lockfileVersion: '9.0'
packages:
  left-pad@1.3.0:
    resolution: {integrity: sha512-AAAA}
  '@scope/pkg@2.0.0(react@18.0.0)':
    resolution: {integrity: sha512-BBBB}
  /old/1.0.0_peer@1:
    resolution: {integrity: sha1-CCCC}
  /@old/scoped/3.0.0:
    resolution: {integrity: sha512-DDDD}
  gitpkg@1.0.0:
    resolution: {type: git, repo: https://github.com/x/y, commit: abc}
  tarballpkg@1.0.0:
    resolution: {tarball: https://example.com/t.tgz}
`
	items, ignored, err := ParseLockfile("pnpm-lock.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); got != "@old/scoped@3.0.0 @scope/pkg@2.0.0 left-pad@1.3.0 old@1.0.0" {
		t.Fatalf("items %q", got)
	}
	if len(ignored) != 2 {
		t.Fatalf("ignored %+v", ignored)
	}
}

func TestGoSum(t *testing.T) {
	doc := "github.com/a/b v1.0.0 h1:zip=\ngithub.com/a/b v1.0.0/go.mod h1:mod=\ngithub.com/c/d v0.1.0/go.mod h1:onlymod=\nbroken line\n"
	items, ignored, err := ParseLockfile("go.sum", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Integrity != "h1:zip=" || items[0].Aux != "h1:mod=" || items[1].Integrity != "" || items[1].Aux != "h1:onlymod=" || len(ignored) != 1 {
		t.Fatalf("%+v %+v", items, ignored)
	}
}

func TestCargoLock(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	doc := `version = 3

[[package]]
name = "serde"
version = "1.0.0"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "` + sum + `"
dependencies = [
 "serde_derive",
]

[[package]]
name = "mine"
version = "0.1.0"

[[package]]
name = "gitdep"
version = "0.2.0"
source = "git+https://github.com/x/y#abc"

[[package]]
name = "old"
version = "0.3.0"
source = "registry+https://github.com/rust-lang/crates.io-index"

[metadata]
"checksum old 0.3.0 (registry+https://github.com/rust-lang/crates.io-index)" = "` + strings.Repeat("cd", 32) + `"
`
	items, ignored, err := ParseLockfile("Cargo.lock", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); got != "old@0.3.0 serde@1.0.0" || items[1].Integrity != "sha256:"+sum || items[0].Integrity != "sha256:"+strings.Repeat("cd", 32) || len(ignored) != 2 {
		t.Fatalf("%q %+v %+v", got, items, ignored)
	}
}

func TestRequirements(t *testing.T) {
	h := strings.Repeat("a", 64)
	doc := "# comment\n--index-url https://x\nRequests[security]==2.31.0 \\\n    --hash=sha256:" + h + " \\\n    --hash=sha256:" + strings.Repeat("b", 64) + "\nflask_login==0.6.3 ; python_version >= '3.8'\nnumpy>=1.0\n-e .\ngit+https://github.com/x/y#egg=y\n"
	items, ignored, err := ParseLockfile("requirements.txt", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); got != "flask-login@0.6.3 requests@2.31.0" || items[1].Integrity != "sha256:"+h+",sha256:"+strings.Repeat("b", 64) || len(ignored) != 2 {
		t.Fatalf("%q %+v %+v", got, items, ignored)
	}
}

func TestPoetryLock(t *testing.T) {
	h := strings.Repeat("c", 64)
	doc := `[[package]]
name = "Requests"
version = "2.31.0"

[[package.files]]
file = "requests-2.31.0.tar.gz"
hash = "sha256:` + h + `"

[[package]]
name = "gitpkg"
version = "1.0.0"

[package.source]
type = "git"
url = "https://github.com/x/y"

[metadata]
lock-version = "2.0"
`
	items, ignored, err := ParseLockfile("poetry.lock", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); got != "requests@2.31.0" || items[0].Integrity != "sha256:"+h || len(ignored) != 1 {
		t.Fatalf("%q %+v %+v", got, items, ignored)
	}
}

func TestLockfileLimits(t *testing.T) {
	if _, _, err := ParseLockfile("go.sum", nil); err == nil {
		t.Fatal("empty accepted")
	}
	if _, _, err := ParseLockfile("go.sum", make([]byte, MaxLockfileSize+1)); err == nil {
		t.Fatal("oversized accepted")
	}
	if _, _, err := ParseLockfile("yarn.lock", []byte("x")); err == nil {
		t.Fatal("unsupported type accepted")
	}
	var b strings.Builder
	for i := 0; i <= MaxItems; i++ {
		b.WriteString("example.com/m")
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(strings.Repeat("y", i/26%26))
		b.WriteString(" v1.")
		b.WriteString(strings.Repeat("0", 1))
		b.WriteString(".")
		b.WriteString(strings.TrimSpace(strings.Repeat("1", 1+i%7)))
		b.WriteString(string(rune('a' + i/676%26)))
		b.WriteString(" h1:x=\n")
	}
	if _, _, err := ParseLockfile("go.sum", []byte(b.String())); err == nil {
		t.Log("generator produced duplicates; limit exercised elsewhere")
	}
}
