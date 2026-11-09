package shadowsocks

import "testing"

func TestValidatorReplaceUsersPreservesReplayHistory(t *testing.T) {
	current := NewValidator(MethodAES256GCM)
	if err := current.Add("old-password", "old-edge"); err != nil {
		t.Fatal(err)
	}
	salt := []byte("previously-accepted-salt")
	if !current.salts.Check(salt) {
		t.Fatal("fresh salt rejected")
	}
	inFlight := current.snapshot()
	prepared := NewValidator(MethodAES128GCM)
	if err := prepared.Add("new-password", "new-edge"); err != nil {
		t.Fatal(err)
	}
	current.ReplaceUsers(prepared)
	if current.salts.Check(salt) {
		t.Fatal("account publication reset replay history")
	}
	users := current.snapshot()
	if len(users) != 1 || users[0].password != "new-password" || users[0].method.Name != MethodAES128GCM.Name {
		t.Fatalf("published users = %+v", users)
	}
	if len(inFlight) != 1 || inFlight[0].password != "old-password" || inFlight[0].method.Name != MethodAES256GCM.Name {
		t.Fatal("publication modified the snapshot of an in-flight handshake")
	}
	prepared.Remove("new-password")
	if len(current.snapshot()) != 1 {
		t.Fatal("published users share the prepared validator's mutable slice")
	}
}
