package coordination

import (
	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
)

// effectiveRegistrationMode 把遗留行上的空值视为 V1；任何未知的非空值一律
// fail-closed，绝不解释为 V2。
func effectiveRegistrationMode(application Application) (catalog.RegistrationMode, error) {
	switch application.RegistrationMode {
	case "":
		return catalog.LegacyImageV1, nil
	case catalog.LegacyImageV1, catalog.BridgingComponentV2, catalog.BuiltinComponentV2:
		return application.RegistrationMode, nil
	default:
		return "", ErrInvalid
	}
}

func effectiveMemberProtocol(member Member) (int, error) {
	switch member.RegistrationProtocol {
	case 0, catalog.RegistrationProtocolV1:
		return catalog.RegistrationProtocolV1, nil
	case catalog.RegistrationProtocolV2:
		return catalog.RegistrationProtocolV2, nil
	default:
		return 0, ErrInvalid
	}
}

// validateReadyInput 为 V1 成员与混合桥接保留旧的镜像/覆盖一致门禁；V2 则校验
// 持久身份、目录版本与注册协议，镜像/覆盖值仅作诊断用途。
func validateReadyInput(application Application, member Member, environment string, input ReadyInput) error {
	if member.Application != application.Application || member.Environment != environment || application.Application == "" {
		return ErrNotReady
	}
	mode, err := effectiveRegistrationMode(application)
	if err != nil {
		return ErrNotReady
	}
	protocol, err := effectiveMemberProtocol(member)
	if err != nil {
		return ErrNotReady
	}
	if protocol == catalog.RegistrationProtocolV1 {
		if mode == catalog.BuiltinComponentV2 {
			return ErrNotReady
		}
		if input.Protocol != runtime.Protocol || input.CoverageDigest != member.CoverageDigest || input.ImageDigest != member.ImageDigest {
			return ErrNotReady
		}
		return nil
	}
	if mode == catalog.LegacyImageV1 {
		return ErrNotReady
	}
	if application.CatalogVersion <= 0 || member.CatalogVersion != application.CatalogVersion || member.CatalogVersion != catalog.Version {
		return ErrNotReady
	}
	if input.Protocol != catalog.RegistrationProtocolV2 || input.CatalogVersion != member.CatalogVersion {
		return ErrNotReady
	}
	return nil
}

func normalizeSpecRegistration(spec ServiceSpec) ServiceSpec {
	if spec.RegistrationProtocol == 0 {
		spec.RegistrationProtocol = catalog.RegistrationProtocolV1
	}
	if spec.RegistrationProtocol == catalog.RegistrationProtocolV1 {
		spec.CatalogVersion = 0
	}
	return spec
}

func validSpecRegistration(spec ServiceSpec) bool {
	protocol := spec.RegistrationProtocol
	if protocol == 0 {
		protocol = catalog.RegistrationProtocolV1
	}
	switch protocol {
	case catalog.RegistrationProtocolV1:
		return digestPattern.MatchString(spec.CoverageDigest) && digestPattern.MatchString(spec.ImageDigest)
	case catalog.RegistrationProtocolV2:
		if spec.CatalogVersion != catalog.Version {
			return false
		}
		if spec.CoverageDigest != "" && !digestPattern.MatchString(spec.CoverageDigest) {
			return false
		}
		if spec.ImageDigest != "" && !digestPattern.MatchString(spec.ImageDigest) {
			return false
		}
		return true
	default:
		return false
	}
}
