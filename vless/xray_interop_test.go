package vless_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestVlessXrayXUDPInterop(t *testing.T) {
	binary := os.Getenv("VNRAY_TEST_XRAY")
	if binary == "" {
		t.Skip("set VNRAY_TEST_XRAY to an Xray executable for interoperability testing")
	}
	a := newXUDPEchoServer(t, 'a')
	b := newXUDPEchoServer(t, 'b')
	runVlessPlugin(t, false, func(server net.Conn) {
		_, port, err := net.SplitHostPort(server.RemoteAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		socksPort := freePort(t)
		cfg := fmt.Sprintf(`{
 "log":{"loglevel":"debug"},
 "inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"socks","settings":{"auth":"noauth","udp":true}}],
 "outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"127.0.0.1","port":%s,"users":[{"id":"%s","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"none"},"mux":{"enabled":true,"concurrency":8,"xudpConcurrency":8}}]
}`, socksPort, port, e2eUUID)
		dir := t.TempDir()
		configPath := filepath.Join(dir, "xray.json")
		if err := os.WriteFile(configPath, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}
		log, err := os.Create(filepath.Join(dir, "xray.log"))
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		cmd := exec.Command(binary, "run", "-config", configPath)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		defer func() {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Xray process did not stop")
			}
			if t.Failed() {
				content, _ := os.ReadFile(log.Name())
				t.Logf("Xray log:\n%s", content)
			}
		}()
		var socks net.Conn
		deadline := time.Now().Add(5 * time.Second)
		for {
			socks, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort)), 100*time.Millisecond)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Xray SOCKS listener did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		defer socks.Close()
		_ = socks.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := socks.Write([]byte{5, 1, 0}); err != nil {
			t.Fatal(err)
		}
		var greeting [2]byte
		if _, err := io.ReadFull(socks, greeting[:]); err != nil || greeting != [2]byte{5, 0} {
			t.Fatalf("SOCKS greeting = %x, %v", greeting, err)
		}
		if _, err := socks.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
		var associate [10]byte
		if _, err := io.ReadFull(socks, associate[:]); err != nil || associate[1] != 0 || associate[3] != 1 {
			t.Fatalf("SOCKS UDP associate = %x, %v", associate, err)
		}
		relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(associate[8])<<8 | int(associate[9])}
		udp, err := net.DialUDP("udp4", nil, relay)
		if err != nil {
			t.Fatal(err)
		}
		defer udp.Close()
		_ = udp.SetDeadline(time.Now().Add(10 * time.Second))
		for i, dest := range []*net.UDPAddr{a, b, a} {
			packet := append([]byte{0, 0, 0, 1}, dest.IP.To4()...)
			packet = append(packet, byte(dest.Port>>8), byte(dest.Port), byte(i))
			if _, err := udp.Write(packet); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64)
			n, err := udp.Read(buf)
			if err != nil {
				t.Fatalf("Xray UDP request %d: %v", i, err)
			}
			marker := byte('a')
			if dest == b {
				marker = 'b'
			}
			want := append(append([]byte(nil), packet[:10]...), marker, byte(i))
			if !bytes.Equal(buf[:n], want) {
				t.Fatalf("SOCKS response = %x, want %x", buf[:n], want)
			}
		}
	})
}
