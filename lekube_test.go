package main

import (
	"reflect"
	"testing"
)

func TestConfigLoadGoldenPath(t *testing.T) {
	cl, err := newConfLoader("./testdata/test.json")
	if err != nil {
		t.Fatal(err)
	}
	c := cl.Get()
	email := "fake@example.com"
	if c.Email != email {
		t.Errorf("email: want %#v, got %#v", email, c.Email)
	}
	if !*c.UseProd {
		t.Errorf("use_prod: want %t, got %t", true, *c.UseProd)
	}
	if !c.LocalDebugOnly {
		t.Errorf("local_debug_only: want %t, got %t", true, c.LocalDebugOnly)
	}
	defaultNS := "default"
	stagingNS := "staging"
	emptyNS := ""
	secs := []*secretConf{
		{
			Namespace: &defaultNS,
			Name:      "test",
			Domains:   []string{"example.com"},
		},
		{
			Namespace: &defaultNS,
			Name:      "missingtest",
			UseRSA:    true,
			Domains:   []string{"www.example.com", "alt.example.com"},
		},
		{
			Namespace: &stagingNS,
			Name:      "missingtest",
			Domains:   []string{"test.example.com"},
		},
		{
			Namespace: &emptyNS,
			Name:      "nonamespace",
			Domains:   []string{"test.example.com"},
		},
	}

	if len(c.Secrets) != len(secs) {
		t.Fatalf("secrets: want %d secrets, got %d", len(secs), len(c.Secrets))
	}
	for i, sec := range secs {
		if !reflect.DeepEqual(sec, c.Secrets[i]) {
			t.Errorf("secret %d: want %#v, got %#v", i, sec, c.Secrets[i])
		}
	}
}
