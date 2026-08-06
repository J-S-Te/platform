package application

import "testing"

func TestSubsystemServiceRoute(t *testing.T) {
	instance, err := SelectSubsystemServiceRoute([]SubsystemServiceInstance{{ServiceRole: "business", Status: SubsystemServiceStatusHealthy, Protocol: "http", InternalHost: "orders-api", InternalPort: 8080}}, "business")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := instance.UpstreamURL(); err != nil || got != "http://orders-api:8080" {
		t.Fatalf("route = %q, err = %v", got, err)
	}
}

func TestSubsystemServiceRouteSkipsUnavailable(t *testing.T) {
	if _, err := SelectSubsystemServiceRoute([]SubsystemServiceInstance{{ServiceRole: "business", Status: SubsystemServiceStatusUnavailable, Protocol: "http", InternalHost: "orders-api", InternalPort: 8080}}, "business"); err == nil {
		t.Fatal("expected unavailable route to be rejected")
	}
}
