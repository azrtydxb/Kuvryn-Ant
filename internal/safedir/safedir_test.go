package safedir

import (
	"os"
	"testing"
)

func mode(t *testing.T, m os.FileMode) os.FileInfo {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, m); err != nil {
		t.Fatal(err)
	}
	i, err := os.Lstat(d)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestPrivate(t *testing.T) {
	for _, c := range []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"owner only", 0o700, true},
		{"fsGroup setgid group-writable (own group)", 0o775&^0o005 | os.ModeSetgid, true},
		{"group readable", 0o750, true},
		{"world readable", 0o705, false},
		{"world writable", 0o777, false},
		{"group and world writable", 0o772, false},
	} {
		if got := Private(mode(t, c.mode)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPrivateRejectsFile(t *testing.T) {
	f := t.TempDir() + "/f"
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	i, _ := os.Lstat(f)
	if Private(i) {
		t.Fatal("file accepted")
	}
}

func TestOwnGroup(t *testing.T) {
	if !ownGroup(uint32(os.Getegid())) {
		t.Fatal("own gid rejected")
	}
	if ownGroup(uint32(os.Getegid()) + 100000) {
		t.Fatal("foreign gid accepted")
	}
}
