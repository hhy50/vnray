package shadowsocks

import (
	"net"
	"testing"
	"time"
)

func TestServerMixedMethods(t *testing.T) {
	validator := NewValidator(MethodAES256GCM)
	methods := []Method{MethodAES256GCM, MethodChaCha20, MethodAES128GCM}
	for _, method := range methods {
		if err := validator.AddWithMethod(method.Name+"-password", "edge", method.Name); err != nil {
			t.Fatal(err)
		}
	}
	_, addr := pipePair(t, validator, &echoTarget{})
	for _, method := range methods {
		t.Run(method.Name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			c := dialSS(t, conn, method, method.Name+"-password", Target{Host: "127.0.0.1", Port: 80}, nil)
			c.write([]byte("mixed cipher echo"))
			if got := string(c.read()); got != "mixed cipher echo" {
				t.Fatalf("echo = %q", got)
			}
		})
	}
	if err := validator.AddWithMethod("aes-256-gcm-password", "changed-edge", MethodAES128GCM.Name); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	c := dialSS(t, conn, MethodAES128GCM, "aes-256-gcm-password", Target{Host: "127.0.0.1", Port: 80}, []byte("updated"))
	if got := string(c.read()); got != "updated" {
		t.Fatalf("updated echo = %q", got)
	}
	if err := validator.AddWithMethod("bad", "edge", "rc4-md5"); err == nil {
		t.Fatal("unsupported cipher accepted")
	}
}
