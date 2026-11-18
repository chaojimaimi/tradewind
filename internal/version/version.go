// Package version 持有构建时注入的应用版本号（M1-plan §5.3）。
//
// 仅此一个变量：发布构建经 scripts/build.sh 以
// -ldflags "-X github.com/chaojimaimi/tradewind/internal/version.Version=vX.Y.Z"
// 注入；未注入（go test / go run）时保持 dev 回退值。
package version

// Version 是应用版本号，由构建期 -ldflags -X 注入。
var Version = "dev"
