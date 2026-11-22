package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func BenchmarkBucketUnlimited(b *testing.B) {
	bucket := NewBucket(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bucket.Wait(context.Background(), 16*1024)
	}
}

func BenchmarkRegistryPair(b *testing.B) {
	r := NewRegistry()
	r.Pair("user_auth:alice")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Pair("user_auth:alice")
	}
}

func BenchmarkRegistrySeed(b *testing.B) {
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = "user_auth:" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := NewRegistry()
		for _, key := range keys {
			r.Apply(key, 1024, 1024)
		}
	}
}

// TestBucketThrottles 验证耗尽当前窗口额度后的字节限速。
func TestBucketThrottles(t *testing.T) {
	b := NewBucket(100 * 1024)
	if err := b.Wait(context.Background(), int(quota(100*1024))); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.Wait(context.Background(), 10*1024); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < window/2 || elapsed > 2*time.Second {
		t.Fatalf("10KiB @100KiB/s 等待 %v", elapsed)
	}
}

// TestBucketUnlimited 断言零速率桶直通。
func TestBucketUnlimited(t *testing.T) {
	b := NewBucket(0)
	start := time.Now()
	if err := b.Wait(context.Background(), 10<<20); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("不限速却耗时 %v", elapsed)
	}
}

// TestSetRateImmediate 断言运行中调速率立即生效：不限速直通，重新限速时立即拥有一个满窗口。
func TestSetRateImmediate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBucket(1024)
		b.SetRate(0)
		start := time.Now()
		if err := b.Wait(context.Background(), 1<<20); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("改为不限速后仍耗时 %v", elapsed)
		}

		// 生产路径就是 NewBucket(0) + SetRate(N)：首个窗口必须立即满额，否则首字节白等一个窗口。
		b.SetRate(100 * 1024)
		if err := b.Wait(context.Background(), int(quota(100*1024))); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("重新限速后首个窗口没有立即放行，等了 %v", elapsed)
		}

		done := make(chan error, 1)
		go func() { done <- b.Wait(context.Background(), 1) }()
		synctest.Wait()
		time.Sleep(window - time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("额度未到就放行: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// TestRefillAnchorsWindowGrid 断言长时间空闲后窗口起点仍落在原网格上，不随访问时刻漂移。
func TestRefillAnchorsWindowGrid(t *testing.T) {
	b := NewBucket(1000)
	b.mu.Lock()
	origin := b.last
	b.tokens = 0
	b.mu.Unlock()

	for _, elapsed := range []time.Duration{2550 * time.Millisecond, 3770 * time.Millisecond} {
		b.mu.Lock()
		b.refill(origin.Add(elapsed))
		got := b.last.Sub(origin)
		b.mu.Unlock()
		if want := elapsed - elapsed%window; got != want {
			t.Fatalf("elapsed %v 后窗口起点 = %v, want %v", elapsed, got, want)
		}
	}
}

// TestRegistryApplyAndForget 断言注册表：懒建 Pair、Apply 即时生效、Forget 丢弃状态。
func TestRegistryApplyAndForget(t *testing.T) {
	r := NewRegistry()
	if up, down := r.Limits("user_auth:alice"); up != 0 || down != 0 {
		t.Fatalf("初始限速 = %d/%d, want 0/0", up, down)
	}
	const up, down = 2 << 20, 512 << 10
	r.Apply("user_auth:alice", up, down)
	if gotUp, gotDown := r.Limits("user_auth:alice"); gotUp != up || gotDown != down {
		t.Fatalf("限速 = %d/%d, want %d/%d", gotUp, gotDown, up, down)
	}
	// 另一种协议的同名 key 不串桶。
	if gotUp, _ := r.Limits("uuid:alice"); gotUp != 0 {
		t.Fatalf("跨协议同名 key 串桶: %d", gotUp)
	}
	// 置零 = 不限速。
	r.Apply("user_auth:alice", 0, 0)
	if gotUp, gotDown := r.Limits("user_auth:alice"); gotUp != 0 || gotDown != 0 {
		t.Fatalf("置零后限速 = %d/%d, want 0/0", gotUp, gotDown)
	}
	// Forget 丢掉记录（再查回到零值）。
	r.Apply("uuid:bob", 1<<20, 1<<20)
	r.Forget("uuid:bob")
	if gotUp, _ := r.Limits("uuid:bob"); gotUp != 0 {
		t.Fatalf("Forget 后限速 = %d, want 0", gotUp)
	}
}

// TestPairSharedAcrossSessions 断言同一身份的多条隧道共享一对桶与一份统计：
// 桶宿主是注册表而非任何一条连接/会话，故重连或并开多条隧道都不会各拿一份配额。
func TestPairSharedAcrossSessions(t *testing.T) {
	r := NewRegistry()
	r.Apply("user_auth:alice", 1<<20, 1<<20)

	// 模拟三条隧道（三条会话）各自取一次 Pair。
	p1, p2, p3 := r.Pair("user_auth:alice"), r.Pair("user_auth:alice"), r.Pair("user_auth:alice")
	if p1.Up != p2.Up || p2.Up != p3.Up {
		t.Fatal("同一身份的多条隧道拿到了不同的上行桶（配额会被放大）")
	}
	if p1.Down != p2.Down || p2.Down != p3.Down {
		t.Fatal("同一身份的多条隧道拿到了不同的下行桶（配额会被放大）")
	}
	// 统计也必须是同一份，否则管理台会漏计。
	p1.Stats.UpWrite.Add(1000)
	p2.Stats.UpWrite.Add(2000)
	if got := p3.Stats.UpWrite.Load(); got != 3000 {
		t.Fatalf("跨隧道统计未累计: got %d, want 3000", got)
	}
	// 另一协议的同名身份不得共享。
	if other := r.Pair("uuid:alice"); other.Up == p1.Up {
		t.Fatal("跨协议同名身份共用了桶")
	}
}

func TestBucketWaitCanceled(t *testing.T) {
	for _, rate := range []int64{0, 1} {
		t.Run(strconv.FormatInt(rate, 10), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := NewBucket(rate).Wait(ctx, 1024); !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait = %v, want canceled", err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := NewBucket(1).Wait(ctx, 16*1024); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want deadline", err)
	}
}

// 调速不打断计时器，等待者在下一个窗口边界按新速率重算。
func TestSetRateAppliedToBlockedWaiter(t *testing.T) {
	for _, rate := range []int64{0, 1 << 30} {
		t.Run(strconv.FormatInt(rate, 10), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				bucket := NewBucket(1)
				done := make(chan error, 1)
				go func() { done <- bucket.Wait(context.Background(), 16*1024) }()
				synctest.Wait()
				bucket.SetRate(rate)
				time.Sleep(window - time.Millisecond)
				select {
				case err := <-done:
					t.Fatalf("rate change took effect before the window ended: %v", err)
				default:
				}
				time.Sleep(time.Millisecond)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestBucketConcurrentAggregate(t *testing.T) {
	bucket := NewBucket(10000)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := bucket.Wait(ctx, int(quota(10000))); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { done <- bucket.Wait(ctx, 500) }()
	}
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// 2000 字节要两个窗口（每窗 1000）；下限守住不超发，上限守住没有多睡窗口。
	if elapsed := time.Since(start); elapsed < 2*window-20*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("aggregate quota: 2000 bytes in %v, want about two windows", elapsed)
	}
}

func TestBucketLargeChunkAndRateAccounting(t *testing.T) {
	bucket := NewBucket(10000)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := bucket.Wait(ctx, 12000); err != nil {
		t.Fatal(err)
	}
	// 12000 字节按每窗 1000 发放，至少要跨 11 个窗口边界。
	if time.Since(start) < time.Second {
		t.Fatal("chunk larger than capacity bypassed limit")
	}
	bucket.mu.Lock()
	bucket.tokens = 0
	bucket.last = time.Now().Add(-window)
	bucket.mu.Unlock()
	bucket.SetRate(20000)
	bucket.mu.Lock()
	tokens := bucket.tokens
	bucket.mu.Unlock()
	if want := quota(10000); tokens != want {
		t.Fatalf("old-rate refill = %v, want %v", tokens, want)
	}
}

// TestBucketClaimsPartialTokens 断言令牌不足一轮所需时先扣下已有额度，余额在窗口边界补齐后放行。
func TestBucketClaimsPartialTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBucket(1000)
		b.mu.Lock()
		b.tokens = 40
		b.mu.Unlock()

		done := make(chan error, 1)
		go func() { done <- b.Wait(context.Background(), 100) }()
		synctest.Wait()

		b.mu.Lock()
		tokens := b.tokens
		b.mu.Unlock()
		if tokens != 0 {
			t.Fatalf("partial tokens not claimed: %v left in bucket", tokens)
		}
		select {
		case err := <-done:
			t.Fatalf("wait returned before the window ended: %v", err)
		default:
		}

		time.Sleep(window - time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("wait returned before the window ended: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// TestBucketPartialClaimNotStolen 断言先到者已扣下的额度不会被后到者抢走。
func TestBucketPartialClaimNotStolen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBucket(1000)
		b.mu.Lock()
		b.tokens = 40
		b.mu.Unlock()

		first := make(chan error, 1)
		go func() { first <- b.Wait(context.Background(), 100) }()
		synctest.Wait()

		// 后到者只能等下一个窗口的额度，拿不到先到者已扣下的 40。
		second := make(chan error, 1)
		go func() { second <- b.Wait(context.Background(), 40) }()
		synctest.Wait()
		time.Sleep(window - time.Millisecond)
		select {
		case err := <-second:
			t.Fatalf("later waiter consumed earlier claim: %v", err)
		default:
		}

		time.Sleep(time.Millisecond)
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if err := <-first; err != nil {
			t.Fatal(err)
		}
	})
}

// TestBucketCancelRefundsClaimed 断言取消时把已扣未交付的额度退回桶里。
func TestBucketCancelRefundsClaimed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := NewBucket(1000)
		b.mu.Lock()
		b.tokens = 40
		b.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- b.Wait(ctx, 100) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want canceled", err)
		}

		b.mu.Lock()
		tokens := b.tokens
		b.mu.Unlock()
		if tokens != 40 {
			t.Fatalf("claimed tokens not refunded: got %v, want 40", tokens)
		}
	})
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Pair("user_auth:alice")
				r.Apply("user_auth:alice", int64(j), int64(j))
				r.Limits("user_auth:alice")
				r.Usage("user_auth:alice")
				r.Forget("user_auth:alice")
			}
		}()
	}
	wg.Wait()
}

// TestPairStatsIsolated 断言不同身份的统计互不干扰。
func TestPairStatsIsolated(t *testing.T) {
	r := NewRegistry()
	pa, pb := r.Pair("user_auth:a"), r.Pair("user_auth:b")
	pa.Stats.UpRead.Add(100)
	if pb.Stats.UpRead.Load() != 0 {
		t.Fatal("不同身份的统计串了")
	}
	if pa2 := r.Pair("user_auth:a"); pa2.Stats.UpRead.Load() != 100 {
		t.Fatal("同身份两次取 Pair 统计不连续")
	}
}

// TestRegistryConcurrencyGate 覆盖用户级并发闸：配置 → 占用 → 排队 → 释放 → 移除。
func TestRegistryConcurrencyGate(t *testing.T) {
	r := NewRegistry()
	identity := "vn-v1:alice"
	// 初始不限。
	if err := r.Gate(identity).Acquire(nil); err != nil {
		t.Fatalf("unlimited acquire: %v", err)
	}
	r.Gate(identity).Release()
	if r.Current(identity) != 0 {
		t.Fatalf("current = %d, want 0", r.Current(identity))
	}

	r.Concurrency(identity, 1)
	if r.Gate(identity).Max() != 1 {
		t.Fatalf("max = %d, want 1", r.Gate(identity).Max())
	}
	if err := r.Gate(identity).Acquire(nil); err != nil {
		t.Fatalf("acquire under limit: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Gate(identity).Acquire(nil) }()
	time.Sleep(20 * time.Millisecond)
	if r.Current(identity) != 1 {
		t.Fatalf("current = %d, want 1", r.Current(identity))
	}
	// 释放占位，排队者通过。
	r.Gate(identity).Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued acquire did not unblock after release")
	}
	// Forget 移除闸：新取回的闸回到不限。
	r.Forget(identity)
	if r.Gate(identity).Max() != 0 {
		t.Fatalf("after forget max = %d, want 0", r.Gate(identity).Max())
	}
}
