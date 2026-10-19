package store

// 本文件是查询日志的唯一出口：Open 返回的 Store 统一包一层 tracedStore，
// 每次数据库访问记一行 op/target/dur（成功 Info、失败 Warn）。要调格式、
// 降噪或整体下线日志，只改这个文件。

import (
	"context"
	"strconv"
	"time"

	"github.com/hhy50/vnray/internal/logging"
	"gorm.io/gorm"
)

// traceQuery 记录一次数据库访问：操作名、目标与耗时。err 指向命名返回值，
// 各 defer 语句处的 time.Now() 即方法入口时刻。成功走 Debug 级（config 的
// log.level 调到 debug 才输出），失败仍 Warn——查询错误永远值得看见。
func traceQuery(start time.Time, op, target string, err *error) {
	dur := time.Since(start).Round(time.Microsecond)
	if *err != nil {
		logging.Warnf("store.query op=%s target=%s dur=%s err=%v", op, target, dur, *err)
		return
	}
	logging.Debugf("store.query op=%s target=%s dur=%s", op, target, dur)
}

// tracedStore 是查询日志装饰层：内嵌 Store 接口，未覆写的方法原样透传（如 Close）。
type tracedStore struct {
	Store
}

// GORM 透传底层连接：装饰层自身必须继续满足 GORMSource，管理台靠它拿同库连接。
func (s tracedStore) GORM() *gorm.DB {
	if gs, ok := s.Store.(GORMSource); ok {
		return gs.GORM()
	}
	return nil
}

func (s tracedStore) SetUser(ctx context.Context, b UserBinding) (err error) {
	defer traceQuery(time.Now(), "SetUser", string(b.Protocol)+" "+b.Key, &err)
	return s.Store.SetUser(ctx, b)
}

func (s tracedStore) GetUser(ctx context.Context, p Protocol, key string) (ub UserBinding, ok bool, err error) {
	defer traceQuery(time.Now(), "GetUser", string(p)+" "+key, &err)
	ub, ok, err = s.Store.GetUser(ctx, p, key)
	return ub, ok, err
}

func (s tracedStore) DeleteUser(ctx context.Context, p Protocol, key string) (deleted bool, err error) {
	defer traceQuery(time.Now(), "DeleteUser", string(p)+" "+key, &err)
	return s.Store.DeleteUser(ctx, p, key)
}

func (s tracedStore) ListUsers(ctx context.Context) (out []UserBinding, err error) {
	defer traceQuery(time.Now(), "ListUsers", "", &err)
	return s.Store.ListUsers(ctx)
}

func (s tracedStore) SetClient(ctx context.Context, b ClientBinding) (err error) {
	defer traceQuery(time.Now(), "SetClient", b.Key, &err)
	return s.Store.SetClient(ctx, b)
}

func (s tracedStore) GetClient(ctx context.Context, key string) (cb ClientBinding, ok bool, err error) {
	defer traceQuery(time.Now(), "GetClient", key, &err)
	cb, ok, err = s.Store.GetClient(ctx, key)
	return cb, ok, err
}

func (s tracedStore) DeleteClient(ctx context.Context, key string) (deleted bool, err error) {
	defer traceQuery(time.Now(), "DeleteClient", key, &err)
	return s.Store.DeleteClient(ctx, key)
}

func (s tracedStore) ListClients(ctx context.Context) (out []ClientBinding, err error) {
	defer traceQuery(time.Now(), "ListClients", "", &err)
	return s.Store.ListClients(ctx)
}

func (s tracedStore) SetAuthKey(ctx context.Context, k AuthKey) (err error) {
	defer traceQuery(time.Now(), "SetAuthKey", k.Key, &err)
	return s.Store.SetAuthKey(ctx, k)
}

func (s tracedStore) GetAuthKey(ctx context.Context, key string) (k AuthKey, ok bool, err error) {
	defer traceQuery(time.Now(), "GetAuthKey", key, &err)
	k, ok, err = s.Store.GetAuthKey(ctx, key)
	return k, ok, err
}

func (s tracedStore) DeleteAuthKey(ctx context.Context, key string) (deleted bool, err error) {
	defer traceQuery(time.Now(), "DeleteAuthKey", key, &err)
	return s.Store.DeleteAuthKey(ctx, key)
}

func (s tracedStore) ListAuthKeys(ctx context.Context) (out []AuthKey, err error) {
	defer traceQuery(time.Now(), "ListAuthKeys", "", &err)
	return s.Store.ListAuthKeys(ctx)
}

func (s tracedStore) GetPlugin(ctx context.Context, name string) (st PluginState, ok bool, err error) {
	defer traceQuery(time.Now(), "GetPlugin", name, &err)
	st, ok, err = s.Store.GetPlugin(ctx, name)
	return st, ok, err
}

func (s tracedStore) SetPlugin(ctx context.Context, st PluginState) (err error) {
	defer traceQuery(time.Now(), "SetPlugin", st.Name, &err)
	return s.Store.SetPlugin(ctx, st)
}

func (s tracedStore) ListPlugins(ctx context.Context) (out []PluginState, err error) {
	defer traceQuery(time.Now(), "ListPlugins", "", &err)
	return s.Store.ListPlugins(ctx)
}

func (s tracedStore) AddUserTraffic(ctx context.Context, p Protocol, key string, up, down int64) (err error) {
	defer traceQuery(time.Now(), "AddUserTraffic", userTrafficTarget(p, key, up, down), &err)
	return s.Store.AddUserTraffic(ctx, p, key, up, down)
}

func (s tracedStore) ListUserTraffic(ctx context.Context) (out []UserTraffic, err error) {
	defer traceQuery(time.Now(), "ListUserTraffic", "", &err)
	return s.Store.ListUserTraffic(ctx)
}

func (s tracedStore) DeleteUserTraffic(ctx context.Context, p Protocol, key string) (deleted bool, err error) {
	defer traceQuery(time.Now(), "DeleteUserTraffic", string(p)+" "+key, &err)
	return s.Store.DeleteUserTraffic(ctx, p, key)
}

func (s tracedStore) AddClientTraffic(ctx context.Context, key string, up, down int64) (err error) {
	defer traceQuery(time.Now(), "AddClientTraffic", clientTrafficTarget(key, up, down), &err)
	return s.Store.AddClientTraffic(ctx, key, up, down)
}

func (s tracedStore) ListClientTraffic(ctx context.Context) (out []ClientTraffic, err error) {
	defer traceQuery(time.Now(), "ListClientTraffic", "", &err)
	return s.Store.ListClientTraffic(ctx)
}

func (s tracedStore) DeleteClientTraffic(ctx context.Context, key string) (deleted bool, err error) {
	defer traceQuery(time.Now(), "DeleteClientTraffic", key, &err)
	return s.Store.DeleteClientTraffic(ctx, key)
}

// userTrafficTarget 把流量落库的目标压成一行可读文本。
func userTrafficTarget(p Protocol, key string, up, down int64) string {
	return string(p) + " " + key + " +" + strconv.FormatInt(up, 10) + "/+" + strconv.FormatInt(down, 10)
}

// clientTrafficTarget 语义同 userTrafficTarget。
func clientTrafficTarget(key string, up, down int64) string {
	return key + " +" + strconv.FormatInt(up, 10) + "/+" + strconv.FormatInt(down, 10)
}
