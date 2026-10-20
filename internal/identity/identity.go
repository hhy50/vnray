// Package identity 提供认证通过后的身份值对象，供各协议 Handler 与插件数据面共用。
package identity

// Identity 是认证通过后的身份：Key 为凭据（uuid），Client 为绑定的路由出口。
type Identity struct {
	Key    string
	Client string
}
