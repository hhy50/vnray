package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hhy50/vnray/internal/protocol"
	"github.com/hhy50/vnray/internal/store"
	"github.com/hhy50/vnray/internal/tcp"
)

func main() {
	mode := flag.String("mode", "run", "bind | run")
	server := flag.String("server", "127.0.0.1:9000", "服务端主端口地址")
	key := flag.String("key", "probe-user-key", "user_key")
	client := flag.String("client", "edge-1", "bind 模式：绑定的客户端名")
	dsn := flag.String("dsn", "", "bind 模式：存储 DSN（mysql）")
	driver := flag.String("driver", "mysql", "bind 模式：存储驱动")
	target := flag.String("target", "127.0.0.1:9001", "run 模式：转发目标 host:port")
	workers := flag.Int("workers", 4, "run 模式：并发协程数")
	duration := flag.Duration("duration", 30*time.Second, "run 模式：运行时长（0 = 一直跑）")
	interval := flag.Duration("interval", 200*time.Millisecond, "run 模式：两次请求的间隔")
	timeout := flag.Duration("timeout", 30*time.Second, "run 模式：单次请求读超时")
	flag.Parse()

	switch *mode {
	case "bind":
		bind(*driver, *dsn, *key, *client)
	case "run":
		run(*server, *key, *target, *workers, *duration, *interval, *timeout)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", *mode)
		os.Exit(2)
	}
}

// bind 直接写持久存储注册一条用户绑定（绕过管理台 CSRF，仅供本地探针使用）。
func bind(driver, dsn, key, client string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := store.Open(ctx, driver, dsn)
	if err != nil {
		panic(err)
	}
	defer func() { _ = st.Close() }()
	err = st.SetUser(ctx, store.UserBinding{Key: key, Protocol: store.ProtocolVNV1,
		Name: "userdaily-" + key, Status: store.UserEnabled, Client: client})
	if err != nil {
		panic(err)
	}
	got, ok, err := st.GetUser(ctx, store.ProtocolVNV1, key)
	if err != nil || !ok {
		panic(fmt.Sprintf("verify binding failed: ok=%v err=%v", ok, err))
	}
	fmt.Printf("binding registered: protocol=%s key=%s client=%s\n", got.Protocol, got.Key, got.Client)
}

// dialUser 完成用户认证握手，返回已认证的多路复用会话。
func dialUser(addr, key string) tcp.Session {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		panic(err)
	}
	muxer := tcp.DefaultMuxer()
	user, err := muxer.Client(conn, tcp.DefaultSessionConfig())
	if err != nil {
		panic(err)
	}
	stream, err := user.OpenStream()
	if err != nil {
		panic(err)
	}
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))

	frame, err := protocol.NewUserAuthFrame(protocol.UserAuthRequest{
		Key: key,
		Info: protocol.UserInfo{
			Name: "daily-user", Version: "v-probe", OS: "probeos", Arch: "probearch",
			Hostname: "daily-host", PID: os.Getpid(),
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
	reply, err := protocol.ParseReply(payload)
	if err != nil {
		panic(err)
	}
	if !reply.OK {
		panic("user auth rejected: " + reply.Reason)
	}
	_ = stream.SetDeadline(time.Time{})
	fmt.Printf("user auth ok: key=%s\n", key)
	return user
}

// run 模拟日常使用：workers 个协程并发"开流 → Forward → 发 HTTP 请求 → 读尽响应 → 关流"。
func run(addr, key, target string, workers int, duration, interval, timeout time.Duration) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		panic(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		panic(err)
	}

	user := dialUser(addr, key)
	defer func() { _ = user.Close() }()

	var (
		reqs    atomic.Int64
		bytes   atomic.Int64
		fail    atomic.Int64
		wg      sync.WaitGroup
		stop    = make(chan struct{})
		doneAll = make(chan struct{})
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				n, err := oneRequest(user, host, uint16(port), timeout)
				if err != nil {
					fail.Add(1)
					fmt.Printf("worker %d: request failed: %v\n", id, err)
				} else {
					reqs.Add(1)
					bytes.Add(n)
				}
				select {
				case <-stop:
					return
				case <-time.After(interval):
				}
			}
		}(i)
	}

	// 统计打印协程。
	var statsWG sync.WaitGroup
	statsWG.Add(1)
	go func() {
		defer statsWG.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fmt.Printf("[%6.1fs] requests=%d bytes=%.1fMB fail=%d\n",
					time.Since(start).Seconds(), reqs.Load(),
					float64(bytes.Load())/1e6, fail.Load())
			}
		}
	}()

	if duration > 0 {
		go func() {
			time.Sleep(duration)
			close(stop)
		}()
	}
	<-stop
	wg.Wait()
	statsWG.Wait()
	close(doneAll)
	_ = doneAll
	fmt.Printf("done: requests=%d bytes=%.1fMB fail=%d\n",
		reqs.Load(), float64(bytes.Load())/1e6, fail.Load())
}

// oneRequest 走一条新流完成一次"转发 + 下载"：ForwardResult OK 后发 HTTP GET，
// 读到 EOF（服务端半关闭）或读超时为止，最后主动关流。
func oneRequest(user tcp.Session, host string, port uint16, timeout time.Duration) (int64, error) {
	stream, err := user.OpenStream()
	if err != nil {
		return 0, fmt.Errorf("open stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	frame, err := protocol.NewForwardFrame(protocol.Address{Host: host, Port: port})
	if err != nil {
		return 0, err
	}
	if err := protocol.NewEncoder(stream).WriteFrame(frame); err != nil {
		return 0, fmt.Errorf("write forward: %w", err)
	}
	resp, err := protocol.NewDecoder(stream).ReadFrame()
	if err != nil {
		return 0, fmt.Errorf("read forward result: %w", err)
	}
	payload, err := protocol.ReadPayload(resp)
	if err != nil {
		return 0, err
	}
	reply, err := protocol.ParseReply(payload)
	if err != nil {
		return 0, err
	}
	if !reply.OK {
		return 0, fmt.Errorf("forward rejected: %s", reply.Reason)
	}

	// 进入数据阶段：读方向给总时长上限，防意外挂死；写完请求后清写超时。
	_ = stream.SetDeadline(time.Now().Add(timeout))
	req := "GET /f.bin HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(stream, req); err != nil {
		return 0, fmt.Errorf("write request: %w", err)
	}
	n, err := io.Copy(io.Discard, stream)
	if err != nil {
		return n, fmt.Errorf("read response: %w", err)
	}
	return n, nil
}
