package application

import "testing"

func gatewayStringPointer(value string) *string { return &value }

func TestValidOptionalBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		value *string
		want  bool
	}{
		{name: "unset", value: nil, want: true},
		{name: "portal http", value: gatewayStringPointer("http://portal.internal"), want: true},
		{name: "portal https with path", value: gatewayStringPointer("https://portal.example/root"), want: true},
		{name: "relative rejected", value: gatewayStringPointer("/portal"), want: false},
		{name: "userinfo rejected", value: gatewayStringPointer("https://user:pass@portal.example"), want: false},
		{name: "query rejected", value: gatewayStringPointer("https://portal.example?source=admin"), want: false},
		{name: "fragment rejected", value: gatewayStringPointer("https://portal.example/#admin"), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validOptionalBaseURL(test.value); got != test.want {
				t.Fatalf("validOptionalBaseURL(%v) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestValidOptionalUpstreamURLMatchesGatewayRenderer(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "private ipv4", value: "http://10.0.0.8:8081", want: true},
		{name: "loopback", value: "http://127.0.0.1:8081/api", want: true},
		{name: "ipv6", value: "http://[fd00::8]:8081", want: true},
		{name: "https hostname", value: "https://contracts.internal/v1/api", want: true},
		{name: "zero port rejected", value: "http://10.0.0.8:0", want: false},
		{name: "large port rejected", value: "http://10.0.0.8:65536", want: false},
		{name: "userinfo rejected", value: "http://user:pass@10.0.0.8:8081", want: false},
		{name: "query rejected", value: "http://10.0.0.8:8081?debug=1", want: false},
		{name: "nginx metacharacter rejected", value: "http://10.0.0.8:8081/api;return", want: false},
		{name: "whitespace rejected", value: "http://10.0.0.8:8081/api path", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validOptionalUpstreamURL(&test.value); got != test.want {
				t.Fatalf("validOptionalUpstreamURL(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestValidOptionalPathPrefixMatchesGatewayRenderer(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "simple", value: "/contract", want: true},
		{name: "nested", value: "/business/contract-v2", want: true},
		{name: "safe punctuation", value: "/contract_v2~preview+ops!", want: true},
		{name: "portal root reserved", value: "/", want: false},
		{name: "duplicate slash rejected", value: "/business//contract", want: false},
		{name: "dot segment rejected", value: "/business/./contract", want: false},
		{name: "traversal rejected", value: "/business/../admin", want: false},
		{name: "encoded path rejected", value: "/business/%2e%2e/admin", want: false},
		{name: "query rejected", value: "/contract?debug=1", want: false},
		{name: "nginx metacharacter rejected", value: "/contract;return", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validOptionalPathPrefix(&test.value); got != test.want {
				t.Fatalf("validOptionalPathPrefix(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestValidGatewayTripleConsistent(t *testing.T) {
	baseURL := "http://portal.internal"
	upstreamURL := "http://10.0.0.8:8081"
	pathPrefix := "/contract"

	tests := []struct {
		name       string
		baseURL    *string
		upstream   *string
		pathPrefix *string
		want       bool
	}{
		{name: "legacy base only", baseURL: &baseURL, want: true},
		{name: "logical environment", want: true},
		{name: "complete gateway mapping", baseURL: &baseURL, upstream: &upstreamURL, pathPrefix: &pathPrefix, want: true},
		{name: "upstream without prefix", baseURL: &baseURL, upstream: &upstreamURL, want: false},
		{name: "prefix without upstream", baseURL: &baseURL, pathPrefix: &pathPrefix, want: false},
		{name: "mapping without public base", upstream: &upstreamURL, pathPrefix: &pathPrefix, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validGatewayTripleConsistent(test.baseURL, test.upstream, test.pathPrefix); got != test.want {
				t.Fatalf("validGatewayTripleConsistent() = %v, want %v", got, test.want)
			}
		})
	}
}
