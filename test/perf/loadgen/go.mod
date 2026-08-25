// 独立 module —— 刻意不属于主工程 github.com/unirate/gateway。
//
// 隔离理由：主工程的 `go build ./...` 与 `go test ./...` 在仓库根执行时，
// Go 会跳过含有自己 go.mod 的子目录，因此压测程序无论怎么改都不可能
// 污染主工程的构建与测试结果。这比 build tag 更彻底 ——
// build tag 仍会被 `go vet ./...` 扫描且容易被误加载。
//
// 代价：压测程序需在 test/perf/loadgen 目录内单独构建。
// 该目录零第三方依赖（仅标准库），故无需 go.sum。
module unirate/perf/loadgen

go 1.22
