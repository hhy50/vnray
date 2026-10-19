package cli

// version 由构建参数（-ldflags "-X .../internal/cli.version=x.y.z"）覆盖，
// 默认 dev 用于本地 go run。
const version = "dev"
