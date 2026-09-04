package main

import "testing"

// T3 (x509 SAN): with every listener bound to all interfaces (":50052"),
// the mTLS server cert must still cover the machine's non-loopback IPv4
// addresses — otherwise an agent dialing <server-LAN-IP>:50052 fails
// hostname verification ("certificate is valid for 127.0.0.1, not <ip>")
// and the uplink stream never comes up (logship then pins its batch at
// "no live uplink stream").
func TestMtlsSANs_AllInterfacesBindsCoverHostLANIPs(t *testing.T) {
	lan := hostIPv4s()
	if len(lan) == 0 {
		t.Skip("no non-loopback IPv4 interface on the test host")
	}
	sans := mtlsSANs(":50052", ":50051", ":8080")
	for _, ip := range lan {
		if !containsString(sans, ip) {
			t.Errorf("mtlsSANs(all-interfaces binds) = %v; missing %q — an agent dialing the server by its LAN IP would fail x509 hostname verification", sans, ip)
		}
	}
}

// A-1: explicitly configured extra names (production domain, public IP) are
// added on top of the local defaults.
func TestMtlsSANs_ExplicitEnvSANs(t *testing.T) {
	t.Setenv("RMMWAY_GRPC_MTLS_SANs", "rmm.example.com,203.0.113.9")
	sans := mtlsSANs(":50052")
	for _, want := range []string{"localhost", "127.0.0.1", "rmm.example.com", "203.0.113.9"} {
		if !containsString(sans, want) {
			t.Errorf("mtlsSANs = %v; missing %q", sans, want)
		}
	}
}

// A named bind contributes its host to the SAN — with the port stripped,
// never "host:port" (a junk SAN that matches no dial target).
func TestMtlsSANs_NamedBindStripsPort(t *testing.T) {
	sans := mtlsSANs("10.0.23.15:50052")
	if !containsString(sans, "10.0.23.15") {
		t.Errorf("mtlsSANs = %v; missing explicit bind host 10.0.23.15", sans)
	}
	for _, s := range sans {
		if s == "10.0.23.15:50052" {
			t.Errorf("mtlsSANs leaked the port into a SAN: %v", sans)
		}
	}
}

// RMMWAY_PUBLIC_URL is the authoritative public dial target: its host is
// ALWAYS included in the mTLS SANs (no need to also set
// RMMWAY_GRPC_MTLS_SANs or RMMWAY_DOMAIN).
func TestMtlsSANs_PublicURLEnvarSeedsSANs(t *testing.T) {
	t.Setenv("RMMWAY_PUBLIC_URL", "https://rmm.example.com")
	sans := mtlsSANs(":50052")
	if !containsString(sans, "rmm.example.com") {
		t.Errorf("mtlsSANs = %v; missing the RMMWAY_PUBLIC_URL host rmm.example.com", sans)
	}
}

// RMMWAY_PUBLIC_URL with an IP address also works.
func TestMtlsSANs_PublicURLIP(t *testing.T) {
	t.Setenv("RMMWAY_PUBLIC_URL", "https://203.0.113.10")
	sans := mtlsSANs(":50052")
	if !containsString(sans, "203.0.113.10") {
		t.Errorf("mtlsSANs = %v; missing the RMMWAY_PUBLIC_URL IP 203.0.113.10", sans)
	}
}

// publicURLHost extracts the host from a URL (stripping scheme and port).
func TestPublicURLHost(t *testing.T) {
	tests := []struct {
		url    string
		expect string
	}{
		{"", ""},
		{"https://rmm.example.com", "rmm.example.com"},
		{"http://rmm.example.com:8080", "rmm.example.com"},
		{"https://203.0.113.10", "203.0.113.10"},
		{"https://203.0.113.10:443", "203.0.113.10"},
		{"rmm.example.com", "rmm.example.com"},
		{"rmm.example.com:8080", "rmm.example.com"},
		{" https://rmm.example.com ", "rmm.example.com"},
	}
	for _, tc := range tests {
		if got := publicURLHost(tc.url); got != tc.expect {
			t.Errorf("publicURLHost(%q) = %q, want %q", tc.url, got, tc.expect)
		}
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
