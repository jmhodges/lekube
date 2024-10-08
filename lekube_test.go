package main

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestConfigLoadGoldenPath(t *testing.T) {
	fakeInt := new(atomic.Int64)
	cl, c, err := newConfLoader("./testdata/test.json", fakeInt, fakeInt)
	if err != nil {
		t.Fatal(err)
	}
	c2 := cl.Get()
	if !cmp.Equal(c, c2) {
		t.Errorf("config pointers returned by newConfLoader and Get should be the same but were not")
	}
	email := "fake@example.com"
	if c.Email != email {
		t.Errorf("email: want %#v, got %#v", email, c.Email)
	}
	if !c.UseProd {
		t.Errorf("use_prod: want %t, got %t", true, c.UseProd)
	}
	if !c.AllowRemoteDebug {
		t.Errorf("allow_remote_debug: want %t, got %t", true, c.AllowRemoteDebug)
	}
	expectedCheck := 3 * time.Minute
	if c.ConfigCheckInterval != expectedCheck {
		t.Errorf("config_check_interval: want %s, got %s", expectedCheck, c.ConfigCheckInterval)
	}
	expectedRenewDur := 3 * time.Hour
	if c.StartRenewDur != expectedRenewDur {
		t.Errorf("start_renew_dur: want %s, got %s", expectedRenewDur, c.StartRenewDur)
	}
	defaultNS := "default"
	stagingNS := "staging"

	secs := []*secretConf{
		{
			Namespace: defaultNS,
			Name:      "test",
			Domains:   []string{"example.com"},
		},
		{
			Namespace: defaultNS,
			Name:      "missingtest",
			UseRSA:    true,
			Domains:   []string{"www.example.com", "alt.example.com"},
		},
		{
			Namespace: stagingNS,
			Name:      "missingtest",
			Domains:   []string{"test.example.com"},
		},
	}

	if len(c.Secrets) != len(secs) {
		t.Fatalf("secrets: want %d secrets, got %d", len(secs), len(c.Secrets))
	}
	for i, sec := range secs {
		if !cmp.Equal(sec, c.Secrets[i]) {
			t.Errorf("secret %d: want %#v, got %#v", i, sec, c.Secrets[i])
		}
	}
}

func TestConfigLoadDefaultConfigCheckInterval(t *testing.T) {
	fakeInt := new(atomic.Int64)
	cl, c, err := newConfLoader("./testdata/no_config_check_interval.json", fakeInt, fakeInt)
	if err != nil {
		t.Fatal(err)
	}
	c2 := cl.Get()
	if !cmp.Equal(c, c2) {
		t.Errorf("config pointers returned by newConfLoader and Get should be the same but were not")
	}
	expected := 30 * time.Second
	if c.ConfigCheckInterval != expected {
		t.Errorf("default config_check_interval: want %s, got %s", expected, c.ConfigCheckInterval)
	}
	expectedRenewDur := 504 * time.Hour
	if c.StartRenewDur != expectedRenewDur {
		t.Errorf("default start_renew_dur: want %s, got %s", expectedRenewDur, c.StartRenewDur)
	}
}

func TestDisallowEmptyNamespaceInSecConfig(t *testing.T) {
	fakeInt := new(atomic.Int64)
	_, _, err := newConfLoader("testdata/no_ns.json", fakeInt, fakeInt)
	if err == nil {
		t.Fatal("should have errored but didn't")
	}
	expectedMsg := "no Namespace given for secret config at index 0 in \"secrets\""
	if err.Error() != expectedMsg {
		t.Errorf("want error %#v, got %#v", expectedMsg, err.Error())
	}
}

func TestBlockedRequest(t *testing.T) {
	type testcase struct {
		path       string
		remoteAddr string
		blocked    bool
	}
	tests := []testcase{
		{"/debug", "93.184.216.34", true},
		{"/debug/", "93.184.216.34", true},
		{"/debug/foobar", "93.184.216.34", true},
		{"/", "93.184.216.34", false},
		{"/foobar", "93.184.216.34", false},
		{"/debug", "127.0.0.1", false},
		{"/debug/", "127.0.0.1", false},
		{"/debug/foobar", "127.0.0.1", false},
		{"/", "127.0.0.1", false},
		{"/foobar", "127.0.0.1", false},
	}
	for _, tc := range tests {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.RemoteAddr = tc.remoteAddr + ":1111"
		actual := isBlockedRequest(r)
		if actual != tc.blocked {
			t.Errorf("path %s, remote addr %s: want %t, got %t", tc.path, r.RemoteAddr, tc.blocked, actual)
		}
	}
}
