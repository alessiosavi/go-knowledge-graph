package netguard_test

import (
	"context"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest/netguard"
)

func TestGuardBlocksPrivateAndSpecialRanges(t *testing.T) {
	g := &netguard.Guard{}
	blocked := []string{
		"http://127.0.0.1/admin",
		"http://10.0.0.1/",
		"http://192.168.1.10/x",
		"http://172.16.5.5/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:8080/",
		"http://0.0.0.0/",
	}
	for _, raw := range blocked {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if err := g.CheckURL(context.Background(), u); err == nil {
			t.Errorf("CheckURL(%s) = nil, want blocked", raw)
		}
	}
}

func TestGuardAllowsPublicAddress(t *testing.T) {
	g := &netguard.Guard{}
	u, err := url.Parse("https://1.1.1.1/")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CheckURL(context.Background(), u); err != nil {
		t.Errorf("CheckURL(public) = %v, want allowed", err)
	}
}

func TestGuardRejectsNonHTTP(t *testing.T) {
	g := &netguard.Guard{}
	for _, raw := range []string{"ftp://example.com/x", "file:///etc/passwd"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.CheckURL(context.Background(), u); err == nil {
			t.Errorf("CheckURL(%s) = nil, want scheme error", raw)
		}
	}
}

func TestGuardBlockedSpecialRanges(t *testing.T) {
	blocked := []string{
		"100.64.0.1", // CGNAT
		"192.0.2.7",  // TEST-NET
		"198.18.0.1", // benchmarking
		"255.255.255.255",
		"64:ff9b::7f00:1", // NAT64 embedding 127.0.0.1
		"fec0::1",         // deprecated site-local
	}
	for _, raw := range blocked {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("parse %q", raw)
		}
		if !netguard.IsBlocked(ip) {
			t.Errorf("IsBlocked(%s) = false, want true", raw)
		}
	}
	if netguard.IsBlocked(net.ParseIP("1.1.1.1")) {
		t.Error("IsBlocked(1.1.1.1) = true, want false")
	}
}

func TestGuardCheckDialBlocksResolvedTargets(t *testing.T) {
	g := &netguard.Guard{}
	if err := g.CheckDial("tcp", "127.0.0.1:80"); err == nil {
		t.Error("CheckDial(loopback) = nil, want blocked")
	}
	if err := g.CheckDial("tcp", "[::1]:443"); err == nil {
		t.Error("CheckDial(::1) = nil, want blocked")
	}
	if err := g.CheckDial("tcp", "169.254.169.254:80"); err == nil {
		t.Error("CheckDial(metadata) = nil, want blocked")
	}
	if err := g.CheckDial("tcp", "1.1.1.1:443"); err != nil {
		t.Errorf("CheckDial(public) = %v, want allowed", err)
	}
	if err := g.CheckDial("tcp", "not-an-address"); err == nil {
		t.Error("CheckDial(malformed) = nil, want error")
	}
}

func TestGuardErrorMentionsAddress(t *testing.T) {
	g := &netguard.Guard{}
	u, _ := url.Parse("http://169.254.169.254/")
	err := g.CheckURL(context.Background(), u)
	if err == nil || !strings.Contains(err.Error(), "169.254.169.254") {
		t.Fatalf("error = %v, want the blocked address named", err)
	}
}
