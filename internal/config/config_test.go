package config

import "testing"

func TestNormalizeRoots(t *testing.T) {
	got, err := NormalizeRoots([]string{"SD/", "/Flash"})
	if err != nil || got[0] != "/SD" || got[1] != "/Flash" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]string{{"/"}, {"/SD", "/SD/Games"}, {"/SD", "/SD"}, {}} {
		if _, err := NormalizeRoots(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
