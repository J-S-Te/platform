package application

import "errors"

// SelectSubsystemServiceRoute selects a healthy service for a role without writing gateway
// configuration. Callers can retry with another instance when a container is unavailable.
func SelectSubsystemServiceRoute(instances []SubsystemServiceInstance, serviceRole string) (SubsystemServiceInstance, error) {
	for _, instance := range instances {
		if instance.ServiceRole == serviceRole && instance.Status == SubsystemServiceStatusHealthy {
			if _, err := instance.UpstreamURL(); err == nil {
				return instance, nil
			}
		}
	}
	return SubsystemServiceInstance{}, errors.New("healthy subsystem service route unavailable")
}
