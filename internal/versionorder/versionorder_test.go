package versionorder

import "testing"

func TestLatest(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"1.2.0", "1.10.0", "1.9.9"}, "1.10.0"},
		{[]string{"v2.0.0-rc.1", "v1.9.0"}, "v1.9.0"},
		{[]string{"2.0.0-rc.2", "2.0.0-rc.10"}, "2.0.0-rc.10"},
		{[]string{"3.0", "3.0.1", "3.0.0-beta"}, "3.0.1"},
		{[]string{"zeta", "alpha"}, "zeta"},
		{[]string{"20240101", "1.0"}, "20240101"},
	}
	for _, c := range cases {
		if got, _ := Latest(c.in); got != c.want {
			t.Errorf("Latest(%v)=%q want %q", c.in, got, c.want)
		}
	}
	if _, ok := Latest(nil); ok {
		t.Fatal("empty list has a latest")
	}
}

func TestLess(t *testing.T) {
	if !Less("1.2", "1.10") || Less("1.10", "1.2") || !Less("abc", "1.0") {
		t.Fatal("unexpected ordering")
	}
}
