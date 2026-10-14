package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsPrivate(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		// Loopback.
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		// Private networks: RFC 1918 and IPv6 unique local.
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.10", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// Link-local, with the cloud metadata address.
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"fe80::1", true},
		{"fe80::1%en0", true},
		// Unspecified and "this network".
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"::", true},
		// Multicast.
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"ff02::1", true},
		{"ff05::2", true},
		// Carrier-grade NAT.
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		// IPv4 written as IPv6.
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:10.0.0.1", true},
		// A private IPv4 address inside NAT64 or 6to4.
		{"64:ff9b::127.0.0.1", true},
		{"64:ff9b::a9fe:a9fe", true}, // 169.254.169.254
		{"2002:a00:1::1", true},      // 10.0.0.1
		{"2002:7f00:1::", true},      // 127.0.0.1
		{"64:ff9b::8.8.8.8", false},
		{"2002:808:808::1", false}, // 8.8.8.8

		// Public addresses, some next to a blocked range.
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"104.18.2.115", false},
		{"172.15.255.255", false},
		{"172.32.0.1", false},
		{"192.169.0.1", false},
		{"169.255.0.1", false},
		{"100.63.255.255", false},
		{"100.128.0.1", false},
		{"2606:4700::1111", false},
		{"::ffff:8.8.8.8", false},
	}
	for _, tt := range tests {
		if got := isPrivate(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("isPrivate(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestRefusePrivate(t *testing.T) {
	tests := []struct {
		address string
		want    string // the address in the refusal, or "" for none
	}{
		{"127.0.0.1:80", "127.0.0.1"},
		{"[::1]:443", "::1"},
		{"[::ffff:169.254.169.254]:80", "169.254.169.254"},
		{"10.1.2.3:11434", "10.1.2.3"},
		{"example.com:443", "example.com"},
		{"no port", "no port"},
		{"8.8.8.8:443", ""},
		{"[2606:4700::1111]:443", ""},
	}
	for _, tt := range tests {
		err := refusePrivate("tcp", tt.address, nil)
		if tt.want == "" {
			if err != nil {
				t.Errorf("refusePrivate(%q) = %v, want nil", tt.address, err)
			}
			continue
		}
		pe, ok := err.(*privateError)
		if !ok || pe.ip != tt.want {
			t.Errorf("refusePrivate(%q) = %v, want a refusal of %s", tt.address, err, tt.want)
		}
	}
}

// TestGuardRefusesLoopback calls a real server on 127.0.0.1, by address
// and by name. With the guard on, no request may reach it. The name
// resolves only when the client dials, so this also shows that the
// check runs on the address dialed.
func TestGuardRefusesLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"data": [{"id": "m"}]}`))
	}))
	defer srv.Close()
	byName := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	ctx := context.Background()
	for _, base := range []string{srv.URL, byName} {
		_, err := New(false).ListModels(ctx, base, testKey)
		if err == nil || !strings.Contains(err.Error(), "private") {
			t.Errorf("ListModels(%s) with the guard on: error = %v, want a private address refusal", base, err)
		}
		if err := New(false).Test(ctx, base, testKey, "m"); err == nil || !strings.Contains(err.Error(), "private") {
			t.Errorf("Test(%s) with the guard on: error = %v, want a private address refusal", base, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the server got %d requests, want none", n)
	}

	// With private addresses allowed, the same call goes through.
	if _, err := New(true).ListModels(ctx, srv.URL, testKey); err != nil {
		t.Errorf("ListModels with private addresses allowed: %v", err)
	}
}
