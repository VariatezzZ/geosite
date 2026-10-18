# geosite

每日定时构建 `geosite.dat`：以 [v2fly/domain-list-community](https://github.com/v2fly/domain-list-community) 的**最新数据**为基底，依据 [Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat) 的增强思路，叠加**中国域名**、**被墙域名（GFWList）**与**广告域名**，去重后打包。

## 与官方 `dlc.dat` 的区别

- `cn` 清单额外并入中国域名（`felixonmars/dnsmasq-china-list` 的 `accelerated-domains.china.conf`）。
- `category-ads-all` 清单额外并入 4 份广告域名源（EasyList China + EasyList、AdGuard DNS filter、pgl.yoyo.org、someonewhocares）。
- 新增 `gfwlist` 清单，收录 `gfwlist/gfwlist` 的被墙域名（去重后约 3,000 条）。
- `geolocation-!cn` 通过 `include:gfwlist` 并入 `gfwlist`，即 **= 基底 + gfwlist**。
- **不生成** `china-list` 等重复分段。

## 冲突策略

`cn` 的候选域名会与**展开后的 `geolocation-!cn`**（基底代理表 + `gfwlist`）比对，命中即**不并入 `cn`**，保留在代理表 —— 即「代理优先」。

- `gfwlist` 写入前先与基底 `cn`、基底 `geolocation-!cn` 去重（**基底 `cn` 优先**：冲突的从 `gfwlist` 剔除）；
- 因 `geolocation-!cn` 会 include `gfwlist`，所以**必须先完成 `geolocation-!cn` 的展开，再处理 `cn`**。

这样不会因为第三方「加速表」的语义偏差而把本应走代理的域名强行直连；被墙域名即使有中国 NS 也不会进 `cn`。

## 裸 TLD 过滤

外部源中的单标签规则（如加速表的 `server=/top/114.114.114.114`）一律丢弃。
否则会生成 `domain:top`，让整个 `.top` 后缀（含境外站点）都命中 `cn`；
广告源里的 `0.0.0.0 localhost` 同理，会误伤 `*.localhost`。
整 TLD 的归属只由基底的 `tld-cn` / `tld-!cn` 决定。

## 去重方式

基底数据使用 `include:` 语法组织，因此本项目**不自行解析 include**，而是：

1. 先用上游构建器把基底展开成「已解析 + 已父子去重」的平铺清单
   （`go run ./ --exportlists=cn,geolocation-!cn,category-ads-all`）；
2. `cmd/merge` 先处理 `gfwlist`：解析 GFWList（base64 + AutoProxy 规则），
   减掉与基底 `cn`、基底 `geolocation-!cn` 冲突的域名，写入 `data/gfwlist`；
3. 再处理 `cn`：把 `gfwlist` 折进 `geolocation-!cn` 的排除集（对应给 `data/geolocation-!cn`
   追加 `include:gfwlist`），然后按域名语义比对：
   - 域名 `x` 被根 `r` 覆盖 ⟺ `x == r` 或 `x` 以 `.r` 结尾（`domain:` 语义）；
   - `full:` 仅精确匹配；
   只把「基底没有、且不与展开后的 `geolocation-!cn` 冲突」的域名追加进 `data/cn`；
4. 广告源追加进 `data/category-ads-all`；
5. 最后再交给上游构建器打包，上游会再做一次父子去重。

## 目录结构

```
.
├─ .github/workflows/build.yml   构建流程（定时 + 手动触发 + 构建相关文件变更时自动触发）
├─ cmd/merge/main.go             展开比对、去重、冲突过滤、追加数据
├─ cmd/verify/main.go            产物体检（自带的轻量 protobuf 解析）
├─ lookup/main.go                按域名反查命中的分类（独立模块的本地排障小工具）
└─ config/sources.json           外部源定义（URL / 格式 / 目标清单 / includes / reference）
```

## 本地构建

```bash
# 1. 准备基底与上游构建器
git clone --depth=1 https://github.com/v2fly/domain-list-community community

# 2. 导出基底展开清单
(cd community && go run ./ --outputdir=../base --outputname=base.dat \
  --exportlists=cn,geolocation-!cn,category-ads-all)

# 3. 叠加外部源
go run ./cmd/merge --community ./community --base ./base \
  --config ./config/sources.json --report ./publish/build-info.json

# 4. 打包
(cd community && go run ./ --outputdir=../publish --outputname=geosite.dat)

# 5. 体检
go run ./cmd/verify --dat ./publish/geosite.dat --basedat ./base/base.dat
```

`cmd/merge` 支持 `--dry-run`（只统计不写入）与 `--rawdir`（用本地已下载文件替代联网，便于离线验证）。

## 使用

```
geosite:cn
geosite:category-ads-all
geosite:gfwlist
geosite:geolocation-!cn
```

产物同时提供 `geosite.dat.sha256sum`、`build-info.json`（记录上游 commit 与各源统计）、`verify.json`。

## 按域名反查（lookup）

`lookup/` 是一个独立小工具（自带模块、零第三方依赖），按 v2ray/xray 的匹配语义反查某个域名命中了哪些分类，便于判断该走代理还是直连：

```bash
cd lookup && go build -o lookup .
./lookup -dat ../publish/geosite.dat -v www.google.com
```

输出示例：

```
www.google.com
  - ALPHABET                 domain:google.com
  - CATEGORY-COMPANIES       domain:google.com
  - GEOLOCATION-!CN          domain:google.com
  - GOOGLE                   domain:google.com
```

`-json` 输出结构化结果，`-v` 显示命中的具体规则；不传域名参数时从标准输入逐行读取。详见 `lookup/README.md`。

## 体检与上游同步

每日构建会把上游最新 commit 拉下来，构建后自动体检：

- section 数量、`cn` / `category-ads-all` / `gfwlist` 条目数下限；
- **`geolocation-!cn` = 基底 + `gfwlist`**：无越界域名泄漏，且 `gfwlist` 全部并入；
- 产物体积区间。

任一检查不过 → 构建失败（能否收到通知取决于你的 GitHub 账号通知设置，本仓库未内置告警），同时 `build-info.json` 里带上游 commit SHA，便于回溯定位是哪次上游变更引起。
