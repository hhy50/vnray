package access

import (
	"testing"

	"github.com/hhy50/vnray/internal/store"
)

func TestValidateUserKey(t *testing.T) {
	for _, protocol := range []store.Protocol{store.ProtocolVNV1, store.ProtocolSS} {
		if err := ValidateUserKey(protocol, "any-key"); err != nil {
			t.Fatalf("%s valid key: %v", protocol, err)
		}
		if err := ValidateUserKey(protocol, ""); err == nil {
			t.Fatalf("%s accepted an empty key", protocol)
		}
	}
	for _, protocol := range []store.Protocol{store.ProtocolVMess, store.ProtocolVLESS} {
		if err := ValidateUserKey(protocol, "b831381d-6324-4d53-ad4f-8cda48b30811"); err != nil {
			t.Fatalf("%s valid uuid: %v", protocol, err)
		}
		if err := ValidateUserKey(protocol, "not-a-uuid"); err == nil {
			t.Fatalf("%s accepted a malformed uuid", protocol)
		}
	}
}
