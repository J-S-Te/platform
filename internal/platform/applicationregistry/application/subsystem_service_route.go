package application

import "errors"

// SelectSubsystemServiceRoute 为指定服务角色挑选一个健康实例，纯内存选择、
// 不写任何网关配置。仅接受 Status 为健康的实例且其 UpstreamURL 可解析；
// 全部不可用时返回错误，调用方可据此对单个容器做重试或整体失败。
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
