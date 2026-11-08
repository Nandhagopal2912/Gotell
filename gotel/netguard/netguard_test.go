package netguard

import (
	"errors"
	"net/netip"
	"testing"
)

func TestIsBlocked(t *testing.T) {
	for _, tt := range []struct {
		ip      string
		blocked bool
		why     string
	}{
		{"127.0.0.1", true, "loopback"},
		{"127.255.0.9", true, "loopback range"},
		{"::1", true, "IPv6 loopback"},
		{"10.0.0.5", true, "private"},
		{"172.16.0.1", true, "private"},
		{"172.31.255.255", true, "private"},
		{"192.168.1.10", true, "home network"},
		{"169.254.169.254", true, "cloud metadata service"},
		{"fe80::1", true, "IPv6 link-local"},
		{"fd00::1", true, "IPv6 private (ULA)"},
		{"0.0.0.0", true, "unspecified"},
		{"::", true, "IPv6 unspecified"},
		{"224.0.0.1", true, "multicast"},
		{"100.64.0.1", true, "carrier-grade NAT"},
		{"255.255.255.255", true, "broadcast"},
		{"::ffff:127.0.0.1", true, "IPv4 loopback hidden in IPv6"},
		{"::ffff:192.168.0.1", true, "IPv4 private hidden in IPv6"},
		{"64:ff9b::a00:1", true, "NAT64 to 10.0.0.1"},

		{"8.8.8.8", false, "public"},
		{"93.184.216.34", false, "public"},
		{"172.32.0.1", false, "just outside 172.16.0.0/12"},
		{"2606:4700:4700::1111", false, "public IPv6"},
	} {
		if got := IsBlocked(netip.MustParseAddr(tt.ip)); got != tt.blocked {
			t.Errorf("IsBlocked(%s) = %v, want %v (%s)", tt.ip, got, tt.blocked, tt.why)
		}
	}
}

func TestControl(t *testing.T) {
	if err := Control("tcp4", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public address: %v, want allowed", err)
	}
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:443", "169.254.169.254:80", "garbage"} {
		if err := Control("tcp", addr, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("Control(%q) = %v, want ErrBlocked", addr, err)
		}
	}
}
