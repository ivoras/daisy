package util

import (
	"slices"
	"testing"
	"time"
)

func TestStrIfMap(t *testing.T) {
	m := StrIfMap{
		"s":      "str",
		"i":      float64(42),
		"list":   []any{"a", "b"},
		"badlst": []any{"a", 1.0},
		"hashes": map[string]any{"1": "x", "2": "y"},
	}
	if s, err := m.GetString("s"); err != nil || s != "str" {
		t.Errorf("GetString = %q, %v", s, err)
	}
	if _, err := m.GetString("i"); err == nil {
		t.Error("GetString on a number should fail")
	}
	if _, err := m.GetString("missing"); err == nil {
		t.Error("GetString on a missing key should fail")
	}
	if i, err := m.GetInt("i"); err != nil || i != 42 {
		t.Errorf("GetInt = %d, %v", i, err)
	}
	if i, err := m.GetInt64("i"); err != nil || i != 42 {
		t.Errorf("GetInt64 = %d, %v", i, err)
	}
	if l, err := m.GetStringList("list"); err != nil || !slices.Equal(l, []string{"a", "b"}) {
		t.Errorf("GetStringList = %v, %v", l, err)
	}
	if _, err := m.GetStringList("badlst"); err == nil {
		t.Error("GetStringList with a non-string element should fail")
	}
	if h, err := m.GetIntStringMap("hashes"); err != nil || h[1] != "x" || h[2] != "y" || len(h) != 2 {
		t.Errorf("GetIntStringMap = %v, %v", h, err)
	}
}

func TestStringSetWithExpiry(t *testing.T) {
	ss := NewStringSetWithExpiry(50 * time.Millisecond)
	if ss.Has("a") {
		t.Fatal("empty set has a")
	}
	if ss.TestAndSet("a") {
		t.Fatal("first TestAndSet should report the string as absent")
	}
	if !ss.TestAndSet("a") || !ss.Has("a") {
		t.Fatal("a should be present")
	}
	time.Sleep(80 * time.Millisecond)
	if ss.Has("a") {
		t.Fatal("a should have expired")
	}
	if ss.TestAndSet("a") {
		t.Fatal("an expired entry should be reported absent")
	}
	ss.Add("b")
	time.Sleep(80 * time.Millisecond)
	if n := ss.CheckExpire(); n != 2 {
		t.Fatalf("CheckExpire expired %d entries, want 2", n)
	}
}

func TestSplitAddress(t *testing.T) {
	for _, tc := range []struct {
		in   string
		host string
		port int
		ok   bool
	}{
		{"example.com:2017", "example.com", 2017, true},
		{"[::1]:2017", "[::1]", 2017, true},
		{"example.com", "example.com", 0, true},
		{"example.com:http", "", 0, false},
	} {
		host, port, err := SplitAddress(tc.in)
		if (err == nil) != tc.ok || host != tc.host || port != tc.port {
			t.Errorf("SplitAddress(%q) = %q, %d, %v", tc.in, host, port, err)
		}
	}
}

func TestHashBytesToHexString(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // SHA256("abc")
	if got := HashBytesToHexString([]byte("abc")); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
