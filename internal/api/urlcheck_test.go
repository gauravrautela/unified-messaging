package api

import "testing"

func TestPublicHTTPURL(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool // true = accepted
	}{
		// Accepted: ordinary public endpoints.
		{"https://hooks.example.com/x", true},
		{"http://example.com", true},
		{"https://example.com:8443/path?q=1", true},
		{"https://8.8.8.8/notify", true},
		{"https://[2001:db8::1]/notify", true},
		{"https://x", true}, // bare hostname, resolvable or not, is not our call

		// Rejected: not an absolute http(s) URL.
		{"", false},
		{"not a url", false},
		{"/relative/path", false},
		{"ftp://example.com/x", false},
		{"file:///etc/passwd", false},
		{"javascript:alert(1)", false},
		{"http://", false},

		// Rejected: loopback.
		{"http://127.0.0.1:9/x", false},
		{"http://127.1.2.3/x", false},
		{"http://localhost:8080/done", false},
		{"http://LOCALHOST/done", false},
		{"http://api.localhost/done", false},
		{"http://[::1]:9/x", false},

		// Rejected: link-local, including the cloud metadata endpoint.
		{"http://169.254.169.254/latest/meta-data/", false},
		{"http://[fe80::1]/x", false},

		// Rejected: RFC1918 and unique-local.
		{"http://10.0.0.5/notify", false},
		{"http://172.16.0.1/x", false},
		{"http://172.31.255.254/x", false},
		{"http://192.168.1.1/x", false},
		{"http://[fc00::1]/x", false},

		// Rejected: unspecified and multicast.
		{"http://0.0.0.0/x", false},
		{"http://[::]/x", false},
		{"http://224.0.0.1/x", false},

		// Accepted: 172.32/12 is outside the private block.
		{"http://172.32.0.1/x", true},
	} {
		err := publicHTTPURL(tc.raw)
		if tc.want && err != nil {
			t.Errorf("publicHTTPURL(%q) = %v, want accepted", tc.raw, err)
		}
		if !tc.want && err == nil {
			t.Errorf("publicHTTPURL(%q) = nil, want rejected", tc.raw)
		}
	}
}
