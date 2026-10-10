# DSH → PC → 手机：刷机工具链桥接

DSH 跑在没有 USB 的服务器上，手机插在 Windows PC 上。本项目由两个部分组成：

```
  ┌──────────────────────┐        HTTP /v1 (Bearer token)        ┌──────────────────────────┐
  │ DSH 服务器 (fnOS)     │ ────────────────────────────────────► │ Windows PC               │
  │                      │                                        │  dsh-mobile-bridge.exe   │
  │ 插件 dsh-pc-mobile-  │ ◄──────────────────────────────────── │   ├─ adb server :5037    │
  │ bridge               │        同一局域网，插件主动连 PC       │   ├─ fastboot            │
  │ 7 个 pc_mobile_* 工具 │                                        │   ├─ platform-tools 自举  │
  └──────────────────────┘                                        │   └─ COM 口读写           │
                                                                  └───────────┬──────────────┘
                                                                              │ USB
                                                                        ┌─────▼─────┐
                                                                        │  Android  │
                                                                        │  手机/平板 │
                                                                        └───────────┘
```

- **PC 端**：`dist/dsh-mobile-bridge.exe` —— Go 单文件、零运行时依赖、双击即跑。它持有 adb / fastboot / adb server / COM 口，暴露一个带 Bearer token 的 HTTP API。首次需要 adb 时会自动下载 Google 官方 platform-tools。
- **DSH 端**：**本仓库根目录就是一个可安装的 host-only Cordis bundle**（`package.json` 里声明 `dsh.bundle`，配 `cordis.patch.yml`），零运行时依赖，用全局 `fetch` 调 PC 端的 HTTP API，注册 7 个 `pc_mobile_*` 工具。
- 完整 HTTP 契约见 [`docs/BRIDGE-API.md`](docs/BRIDGE-API.md)。

## 1. 目录

| 路径 | 说明 |
| --- | --- |
| `package.json` | **插件包本体**：`dsh.bundle.patch` → `./cordis.patch.yml`，仓库根目录即可被 `dsh plugin add` 安装 |
| `index.js` `tools.js` `bridge-client.js` `host-adb.js` | 插件实现：7 个工具定义、HTTP 客户端、把本机 adb 路由到 PC 的 wrapper |
| `cordis.patch.yml` | bundle patch：插入 `pc-mobile-bridge` 一行（`bridgeUrl`/`token` 留空也能挂载） |
| `locale/en.json` `locale/zh.json` `icon.svg` | 插件市场里显示的标题、描述与图标 |
| `bridge/` | PC 端 Go 源码（module `dshtools/mobile-bridge`） |
| `dist/dsh-mobile-bridge.exe` | Windows amd64 成品（8 225 280 字节，PE32+，sha256 `8281e400…6757b62`） |
| `dist/dsh-mobile-bridge` | Linux amd64 成品（7 905 440 字节，sha256 `edbc7c53…8efefe62`），本机跑测试用 |
| `docs/BRIDGE-API.md` | HTTP API v1 契约（端点、错误码、字段） |
| `docs/awesome-dsh-plugin-entry.yml` | 投稿到 awesome-dsh-plugin 列表用的条目文件（复制改名后提 PR） |
| `tests/bridge-smoke.sh` | PC 端冒烟：53 项断言 |
| `tests/plugin-smoke.mjs` | 插件端冒烟：69 项断言（真实 HTTP 往返 + 工具 schema 校验） |
| `tests/schema-conformance.mjs` | 用 `@deepseek-ai/dsh-tools` 自带的校验器核对编译出的 JSON Schema：32 项断言 |
| `tools/restart-harness.sh` | 仅开发用：在 fnOS 上重启 DSH 宿主（与本插件功能无关） |

## 2. PC 端：跑起来

1. 把 `dist/dsh-mobile-bridge.exe` 拷到 PC 任意目录（例如 `D:\dsh-bridge\`）。
2. 双击运行（或在 `cmd` 里执行）。首次运行会在 exe 同目录写一份 `dsh-mobile-bridge.json` 并打印：

```
  dsh-mobile-bridge 1.0.0 (windows/amd64)
  listen        : :8787 (dual-stack; config says 0.0.0.0:8787)
  token         : <32 位十六进制>
  config file   : D:\dsh-bridge\dsh-mobile-bridge.json
  platform-tools: <not found> (source=missing)
  adb version   : unavailable (… run the install_platform_tools setup action …)
  serial ports  : COM5 (蓝牙链接上的标准串行 (COM5))
  bridgeUrl     : http://192.168.1.23:8787
  also at       : http://10.0.0.7:8787   (Wi-Fi)
  skipped       : 169.254.19.30 (以太网) — 169.254.x.x means this adapter never got a DHCP lease, so a server on your LAN cannot dial it; use one of the addresses above
```

`bridgeUrl` 不再只是"第一个非回环 IPv4"：它会跳过 `169.254.x.x`（没有 DHCP 租约的网卡）并把这类地址单独作为警告列出，同时把其他候选地址一并打印出来。选错网卡（比如插着网线但没拿到 IP 的以太网 + 有 IP 的 Wi-Fi）时，直接照下面 `also at` 或 `ipconfig` 里的那个 `192.168.x.x` 填。

3. 记下 **token** 和 **bridgeUrl**，填到 DSH 端（见第 3 节）。
4. 首次需要 adb 时，让 DSH 调一次 `pc_mobile_setup`，或直接在 PC 上运行：

```
dsh-mobile-bridge.exe --platform-tools-dir "D:\dsh-bridge\platform-tools"
```

Windows 防火墙第一次会弹窗，**必须允许专用网络入站**，否则 DSH 连不上。

### 配置项 `dsh-mobile-bridge.json`

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `listen` | `0.0.0.0:8787` | HTTP 监听地址；只为本机自测可改 `127.0.0.1:8787` |
| `token` | 自动生成 | Bearer token；首启随机生成并写回配置，之后稳定不变 |
| `platformToolsDir` | 空 | 存放 platform-tools 的目录；空=使用默认下载目录 |
| `adbPort` | `5037` | PC 上 adb server 的端口 |
| `startAdbServerOnLaunch` | `true` | 启动时拉起 `adb -a -P 5037 nodaemon server`（`-a` = 允许局域网连入） |
| `autoDownloadPlatformTools` | `true` | 找不到 adb 时自动下载 Google 官方 zip |
| `allowedRoots` | `[]` | `/v1/fs/*` 可访问的根目录白名单；空=不限制 |
| `logFile` | 空 | 追加写日志文件；空=只写 stdout |

命令行开关（覆盖配置）：`--listen --token --config --platform-tools-dir --adb-port --no-start-adb-server --no-auto-download --log-file --print-config`。

### 语义要点

- **子进程非零退出不是 HTTP 错误**：返回 200，body 里 `exitCode != 0`，并带 `stdout`/`stderr`（各截尾 1 MiB）。命令**启动失败**（找不到 adb、超时）才是 `exitCode: -1` + `error` 字段。
- `POST /v1/adb`、`POST /v1/fastboot` 的 `args` 是**字面 argv 数组**，不是 shell 字符串。
- 二进制一律 base64；请求体上限 64 MiB；`/v1/fs/get` 单次上限 8 MiB。

## 3. DSH 端：插件

**本仓库根目录就是插件包**（`package.json` 里声明了 `dsh.bundle`），直接按包安装即可：

```sh
dsh plugin add https://www.656463.xyz:20236/1045188196/dsh-mobile-bridge   # 或：dsh plugin add /path/to/harness-pc-mobile
```

开发时也可以在本机把仓库目录 `link:` 进 profile（本机用的就是这条路径）：`plugin_manager install_bundle target="<仓库绝对路径>"`。

配置桥接地址，在 profile 的 `cordis.patch.yml`（用户 patch 层）加一条覆盖：

```yaml
- id: pc-mobile-bridge
  name: "dsh-pc-mobile-bridge"
  config:
    bridgeUrl: "http://192.168.1.23:8787"
    token: "<exe 打印的 token>"
```

或者用环境变量（不需要动 YAML）：`DSH_PC_MOBILE_BRIDGE_URL`、`DSH_PC_MOBILE_BRIDGE_TOKEN`。

留空也能挂载：每个 `pc_mobile_*` 工具会直接返回上面这段待填的 YAML，而不是一个网络错误。

> 改完 `cordis.patch.yml` 后，**新增/替换插件模块需要重启 Harness 宿主**才会加载新的 JS 模块代；同一份 patch 文件的配置改动会实时生效。

> ⚠️ **不要用插件市场的开关来"重载改过的插件代码"**。`dshmarket` 的 toggle 对同一 bundle 的源码改动会回报 `activation: live / hot: true`，但它热挂载的是**本进程早先导入的那一代模块**（市场源码里 #685 的注释说的就是这个：*only a restart can*）。实测：改完 `tools.js` 后 toggle "开" → 市场报 live，紧接着 API 立刻报 `Invalid schema for function 'pc_mobile_adb' … got 'type: null'`；重启后才正常。**结论：改过 JS 就必须重启宿主。**

### 7 个工具

| 工具 | 作用 |
| --- | --- |
| `pc_mobile_status` | PC 端体检：bridge 版本、platform-tools 位置与来源、adb server 状态、串口列表、当前 bridge 配置 |
| `pc_mobile_devices` | 列出 adb 设备、fastboot 设备、串口（含是否被占用） |
| `pc_mobile_adb` | 在 PC 上跑一条 adb 命令（`serial` 自动插 `-s <serial>`） |
| `pc_mobile_fastboot` | 在 PC 上跑一条 fastboot 命令；**写入类命令必须带 `confirm: true`** |
| `pc_mobile_file` | PC 端文件操作：`upload` / `download` / `ls` / `sha256`（分块传输并校验 sha256） |
| `pc_mobile_serial` | COM 口：`list` / `open` / `write` / `read` / `close`（MTK、高通下载模式的原始串口） |
| `pc_mobile_setup` | 初始化：`install_platform_tools` / `start_adb_server` / `stop_adb_server` / `refresh` / `install_host_adb` |

`pc_mobile_fastboot` 的破坏性门禁覆盖 `flash`、`flashall`、`erase`、`format`、`update`、`flashing`、`delete`、`*-logical-partition`、`snapshot-update`、`-w`、`set_active`，以及 `oem unlock/lock/format/erase/frp`。未确认时**本地拒绝、不发 HTTP**，并把要重发的调用原样告诉模型。

## 4. 让现有 `android_*` 工具也走 PC

profile 里已装的 `@zseven-w/dsh-android` 提供 `android_devices` / `android_screenshot` / `android_ui_tree` / `android_tap_*` 等一整套面板与 UI 自动化工具，但它们在本机（服务器）找 adb —— 而手机挂在 PC 上，所以默认看不到任何设备。打通方式如下（本机已打通：`android_devices` 能看到真机）。

打通方式：让服务器上的 `adb` 变成一个只做转发的 wrapper，把 adb 协议指向 PC 的 adb server：

```
$HOME/Android/Sdk/platform-tools/adb   ← wrapper
   exec env ADB_SERVER_SOCKET="tcp:<pc>:5037" "<真正的 adb>" "$@"
```

一条命令搞定（`host` / `port` 自动取自插件配置里的 `bridgeUrl`，Linux platform-tools 会下到 `$HOME/Android/Sdk/.dsh-real-platform-tools`）：

```
pc_mobile_setup { action: "install_host_adb" }
```

它会：安装/复用 Linux platform-tools → 写 wrapper → 设 `process.env.ADB` 与 `ADB_SERVER_SOCKET` → 跑一次 `adb devices -l` 自检。

三个实战要点：

- **不会白下载**：`$HOME/Android/Sdk/.dsh-real-platform-tools/adb` 已有可用客户端时会直接复用；也可以用 `clientPath` 指定别处的 adb。
- **别把 `clientPath` 指向 wrapper 自己的路径**（`$HOME/Android/Sdk/platform-tools/adb`）——那会让 wrapper 递归 exec 自己。先把已有的 adb `mv` 到 `.dsh-real-platform-tools/`，再调用 `install_host_adb`（它会自动复用）。
- **通常不需要重启**：`@zseven-w/dsh-android` 在 `apply()` 时解析并缓存的正是 `$HOME/Android/Sdk/platform-tools/adb` 这个路径，wrapper 写上去后当场生效（实测 `android_devices` 立刻看到真机）。只有当它当初解析到的是别处（例如那时这个文件还不存在，退化成了 PATH 上的 `adb`）时，才需要重启一次。

## 5. 典型刷机移植流程

1. `pc_mobile_setup {action:"refresh"}` → `pc_mobile_devices` 确认手机在列。
2. `pc_mobile_adb {args:["shell","getprop","ro.product.model"]}` 之类的读取命令任意跑。
3. 传包：`pc_mobile_file {op:"upload", localPath:"…", remotePath:"D:\\rom\\boot.img"}`（本地 → PC）。
4. 进 bootloader：`pc_mobile_adb {args:["reboot","bootloader"]}`。
5. 刷写：`pc_mobile_fastboot {args:["flash","boot","D:\\rom\\boot.img"], confirm:true}`。
6. 下载模式/串口日志：`pc_mobile_serial {action:"open", port:"COM3", baudRate:115200}` 然后 `action:"read"`。
7. 需要看屏幕/点按：走第 4 节的 adb 路由后直接用 `android_screenshot` / `android_ui_tree` / `android_tap_element`。

## 6. 验证

```bash
bash tests/bridge-smoke.sh            # PC 端（用本机 Linux 二进制）：53 checks, 0 failure(s)
node tests/plugin-smoke.mjs           # 插件端（真实 HTTP）：69 checks, 0 failure(s)
node tests/schema-conformance.mjs     # 工具 schema 与 @deepseek-ai/dsh-tools 逐字节对齐：32 checks
```

`schema-conformance.mjs` 直接 import `@deepseek-ai/dsh-tools`，用它自己的 `assertObjectJsonSchema()` 校验这 7 个工具的 `parameters` 与 `output.schema`，并把本插件编译出的 schema 与该包的 `parameterSchemaSpecToJsonSchema()` 的结果逐字节比对（找不到该包时 SKIP，退出码 0）。

`plugin-smoke.mjs` 会自己挑空闲端口、起一份临时配置的 bridge、再用 `createTools()` 拿到的工具对象跑真实往返（含 700 000 字节分块上传/下载与双向 sha256 比对），并断言每个工具的 `parameters` 是 `type: "object"` 的 JSON Schema —— 这一条是必需的：`ctx.tools.register()` 收的是**已编译**的 JSON Schema，直接登记本插件内部的“逐属性方言”会让 API 拒掉整个回合（`got 'type: null'`）。

## 7. 排错

| 现象 | 原因 / 处理 |
| --- | --- |
| 工具报 401 `unauthorized` | token 不匹配。用 `--print-config` 看 PC 端实际 token |
| 工具报连不上 / ECONNREFUSED | bridge 没跑、端口被占、或 Windows 防火墙拦了入站 |
| `bridgeUrl` 是 `169.254.x.x`，或 DSH 侧连接超时 | PC 选错了网卡（那张网卡没有 DHCP 租约）。看启动 banner 的 `also at` / `skipped` 两行，或在 PC 上 `ipconfig` 找有默认网关的那个 `192.168.x.x`，用它替换配置里的 `bridgeUrl` |
| exe 起不来且日志写 `bind: address already in use` | 8787 被别的程序占了，改 `--listen 0.0.0.0:8788` |
| `pc_mobile_devices` 里 `adbError` 非空 | PC 上没有 adb：`pc_mobile_setup {action:"install_platform_tools"}` |
| `platformTools.source` 是 `missing` | 同上；`source` 取值 `config` / `path` / `autodownload` / `missing` |
| `path_not_allowed` (403) | `/v1/fs/*` 越出 `allowedRoots` |
| `session_unknown` (404) | 串口 sessionId 已失效（关过或重启过 bridge） |
| `port_busy` (409) | COM 口已被别的程序（烧录工具、串口助手）占用 |
| fastboot 被拒 | 写入类命令要 `confirm: true` |
| 工具列表里没有 `pc_mobile_*` | ① profile `package.json` 的 `dsh.profile.bundles` 里没有它；② dshmarket 的 `.dsh-market/state.json` 的 `disabled` 数组还压着它。两者都修好后**重启宿主** |
| 报 `Invalid schema for function 'pc_mobile_adb' … got 'type: null'` | 宿主仍在跑改代码之前那一代模块：**重启宿主**。此时插件市场 toggle 回报的 `activation: live/hot: true` 是假信号 |
| 手机显示为 COM 口但 `adb devices` 看不到 | 那是下载模式，不是 adb/fastboot；用 `pc_mobile_serial` |

## 8. 从源码构建

服务器上需要 workspace 内的 GOROOT（`/tmp` 不可写，所有临时目录都要落在 workspace 里）：

```bash
cd bridge
export GOROOT="/vol1/1000/Ai workspaces/deepseek harness/harness-pc-mobile/.toolchain/go"
export PATH="$GOROOT/bin:$PATH"
export GOCACHE="$(pwd)/.gocache" GOPATH="$(pwd)/.gopath" TMPDIR="$(pwd)/.gotmp"
export GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org,direct
rm -f ../dist/dsh-mobile-bridge ../dist/dsh-mobile-bridge.exe   # 旧产物无写位，必须先删
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../dist/dsh-mobile-bridge.exe .
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../dist/dsh-mobile-bridge .
```

依赖只有 `go.bug.st/serial`（串口）与 `golang.org/x/sys`。

## 9. 已知限制

- 插件是 host-only：没有客户端 UI，只在插件清单里占一条记录（`locale/en.json`、`locale/zh.json`、`icon.svg` 提供标题/描述/图标）。
- PC 端没有鉴权以外的加固：token 是唯一凭据，bridge 监听 `0.0.0.0` 时请只放在可信局域网里。
- `adb -a` 会把 PC 的 adb server 暴露给局域网；不想如此就设 `adbPort` 指向一个只监听本机的 server，并在 bridge 上关掉 `startAdbServerOnLaunch`。
- 串口 `read` 是“排空至安静”语义：收到数据后端口一安静就返回；`timedOut` 只在一个字节都没收到时为 true。
- `pc_mobile_adb` / `pc_mobile_fastboot` 等长命令的工具超时已放宽（最长 1800 s），但 DSH 侧的工具超时策略仍可能先到。
