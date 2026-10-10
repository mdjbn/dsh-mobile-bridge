# 预编译成品

这两个文件是从本仓库的 Go 源码（`../bridge/`）交叉编译出来的原样产物，**没有**任何手工修改。仓库里放成品是因为最终用户多数不会装 Go；如果你不放心，按下面的命令自己编一份，或先用 `sha256sum` 核对。

| 文件 | 目标平台 | 字节 | SHA-256 |
| --- | --- | --- | --- |
| `dsh-mobile-bridge.exe` | windows/amd64（PE32+ console） | 8225280 | `8281e4009a75a8f0a9e13532942f790503efcea430c1d4e2e04f31bd87657b62` |
| `dsh-mobile-bridge` | linux/amd64 | 7905440 | `edbc7c536c198da39a8476d2adfb6889b7dc33ee4465579059b4f7cd8efefe62` |

Windows 那份是给用户双击运行的（免安装，首次自动下载 platform-tools）；Linux 那份只用于在本机跑 `tests/`（`tests/plugin-smoke.mjs` 会启动它）。

## 自己编译

需要 Go 1.25+：

```sh
cd bridge
export GOCACHE="$PWD/.gocache" GOPATH="$PWD/.gopath" GOFLAGS=-mod=mod
export TMPDIR="$PWD/.gotmp"          # 必须指向可写目录，见下
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../dist/dsh-mobile-bridge.exe .
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../dist/dsh-mobile-bridge .
```

- `TMPDIR` 一定要改：有些系统（例如 fnOS）的 `/tmp` 对非 root 不可写，`go build` 会失败并报 `/tmp/go-build…/a.out: permission denied`。
- 如果目标文件已存在却没有写位，`go build -o` 会报 `already exists and is not an object file`，先 `rm -f ../dist/dsh-mobile-bridge*` 再编。
- Windows 目标用 `CGO_ENABLED=0` 交叉编译即可，Go 的 `go.bug.st/serial` 会把 COM 口走 Win32 API，不需要交叉编译器。
