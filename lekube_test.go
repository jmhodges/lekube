package main

import (
	"context"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestConfigLoadGoldenPath(t *testing.T) {
	fakeInt := new(atomic.Int64)
	cl, c, err := newConfLoader("./testdata/test.json", fakeInt, fakeInt)
	if err != nil {
		t.Fatal(err)
	}
	c2 := cl.Get()
	if c != c2 {
		t.Errorf("config pointers returned by newConfLoader and Get should be the same but were not")
	}
	email := "fake@example.com"
	if c.Email != email {
		t.Errorf("email: want %#v, got %#v", email, c.Email)
	}
	if !*c.UseProd {
		t.Errorf("use_prod: want %t, got %t", true, *c.UseProd)
	}
	if !c.AllowRemoteDebug {
		t.Errorf("allow_remote_debug: want %t, got %t", true, c.AllowRemoteDebug)
	}
	expectedCheck := jsonDuration(3 * time.Minute)
	if c.ConfigCheckInterval != expectedCheck {
		t.Errorf("config_check_interval: want %s, got %s", expectedCheck, c.ConfigCheckInterval)
	}
	expectedRenewDur := jsonDuration(3 * time.Hour)
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
		if !reflect.DeepEqual(sec, c.Secrets[i]) {
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
	if c != c2 {
		t.Errorf("config pointers returned by newConfLoader and Get should be the same but were not")
	}
	expected := jsonDuration(30 * time.Second)
	if c.ConfigCheckInterval != expected {
		t.Errorf("default config_check_interval: want %s, got %s", expected, c.ConfigCheckInterval)
	}
	expectedRenewDur := jsonDuration(504 * time.Hour)
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

// TestOtelDroppingContextData is a test that demonstrates how the v1.21.0 and
// earlier OpenTelemetry sdk/metrics package drops data when the Context is
// already canceled. It's a reminder to me (and others) to use a different
// context for recording error states. This was fixed in versions of
// OpenTelemetry later than v1.21.0. See
// https://github.com/open-telemetry/opentelemetry-go/commit/8e756513a630cc0e80c8b65528f27161a87a3cc8
// When this test fails, change recordErrorMetric to take a Context as an
// argument instead of creating its own, fix the context.TODOs elsewhere, and
// remove this test completely.
func TestOtelDroppingContextData(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(context.Background())

	scopeName := t.Name()
	counterName := "ticks"
	meter := provider.Meter(scopeName)

	tick, err := meter.Int64Counter(counterName)
	if err != nil {
		t.Fatalf("unable to make the ticks counter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// The meat of the work is adding to the counter with a live Context and
	// then attempting to add to it with a canceled Context.
	tick.Add(ctx, 1)
	cancel()
	tick.Add(ctx, 1)

	rm := new(metricdata.ResourceMetrics)
	err = reader.Collect(context.Background(), rm)
	if err != nil {
		t.Fatalf("failed to collect metrics: %v", err)
	}

	scopeIndex := slices.IndexFunc(rm.ScopeMetrics, func(sm metricdata.ScopeMetrics) bool { return sm.Scope.Name == scopeName })
	if scopeIndex == -1 {
		t.Fatalf("expected to find a ScopeMetrics with the name %s, got none", scopeName)
	}
	scope := rm.ScopeMetrics[scopeIndex]
	metricIndex := slices.IndexFunc(scope.Metrics, func(m metricdata.Metrics) bool { return m.Name == counterName })
	onlyMetric := scope.Metrics[metricIndex]
	onlySum, ok := onlyMetric.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("expected our ticks Aggregation to be a metricdata.Sum[int64], got %T", onlyMetric.Data)
	}
	datapoint := onlySum.DataPoints[0]
	if datapoint.Value != 1 {
		t.Errorf("expected our ticks Aggregation to have a value of 1 because the second add is dropped since its Context is errored out, got %d", datapoint.Value)
	}

}
