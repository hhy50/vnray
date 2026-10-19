package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/tcp"
)

func main() {
	addr, key := os.Args[1], os.Args[2]

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		panic(err)
	}
	muxer := tcp.DefaultMuxer()
	user, err := muxer.Client(conn, tcp.DefaultSessionConfig())
	if err != nil {
		panic(err)
	}
	defer func() { _ = user.Close() }()

	stream, err := user.OpenStream()
	if err != nil {
		panic(err)
	}
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))

	frame, err := protocol.NewUserAuthFrame(protocol.UserAuthRequest{
		Key: key,
		Info: protocol.UserInfo{
			Name: "probe-user", Version: "v-probe", OS: "probeos", Arch: "probearch",
			Hostname: "probe-host", PID: os.Getpid(),
		},
	})
	if err != nil {
		panic(err)
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		panic(err)
	}
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		panic(err)
	}
	payload, err := protocol.ReadPayload(resp)
	if err != nil {
		panic(err)
	}
	fmt.Println("AuthResult payload:", string(payload))
}
