package access

import (
	"errors"

	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/vmess"
)

// ValidateUserKey 校验用户密钥是否满足对应入站协议的要求。
func ValidateUserKey(p store.Protocol, key string) error {
	if p.UsesUUID() {
		return vmess.ValidateUUID(key)
	}
	if key == "" {
		return errors.New("access: user key must not be empty")
	}
	return nil
}
