# geosite

每日定时构建 `geosite.dat`：以 [v2fly/domain-list-community](https://github.com/v2fly/domain-list-community) 的**最新数据**为基底，叠加**中国域名**与**广告域名**，去重后打包。

## 与官方 `dlc.dat` 的区别

- `cn` 清单额外并入中国域名（`felixonmars/dnsmasq-china-list` 的 `accelerated-domains.china.conf`）。
- `category-ads-all` 清单额外并入 4 份广告域名源（EasyList China + EasyList、AdGuard DNS filter、pgl.yoyo.org、someonewhocares）。
- **不生成** `china-list` 等重复分段。
- `geolocation-!cn` 保持与基底完全一致，不做任何改动。

## 冲突策略

与基底 `geolocation-!cn`（代理表）冲突的域名**不并入 `cn`**，一律保留在代理表 —— 即「基底优先」。这样不会因为第三方「加速表」的语义偏差而把本应走代理的域名强行直连。

## 去重方式

基底数据使用 `include:` 语法组织，因此本项目**不自行解析 include**，而是：

1. 先用上游构建器把基底展开成「已解析 + 已父子去重」的平铺清单
   （`go run ./ --exportlists=cn,geolocation-!cn,category-ads-all`）；
2. 本项目的 `cmd/merge` 按域名语义比对：
   - 域名 `x` 被根 `r` 覆盖 ⟺ `x == r` 或 `x` 以 `.r` 结尾（`domain:` 语义）；
   - `full:` 仅精确匹配；
3. 只把「基底没有、且不与代理表冲突」的域名追加进 `data/cn` 与 `data/category-ads-all`；
4. 最后再交给上游构建器打包，上游会再做一次父子去重。

## 目录结构

```
.
├─ .github/workflows/build.yml   构建流程（定时 + 手动触发 + 构建相关文件变更时自动触发）
├─ cmd/merge/main.go             展开比对、去重、冲突过滤、追加数据
├─ cmd/verify/main.go            产物体检（自带的轻量 protobuf 解析）
└─ config/sources.json           外部源定义（URL / 格式 / 目标清单）
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
geosite:geolocation-!cn
```

产物同时提供 `geosite.dat.sha256sum`、`build-info.json`（记录上游 commit 与各源统计）、`verify.json`。

## 体检与上游同步

每日构建会把上游最新 commit 拉下来，构建后自动体检：

- section 数量、`cn` / `category-ads-all` 条目数下限；
- **`geolocation-!cn` 与基底逐条一致**（「基底优先」策略的不变式）；
- 产物体积区间。

任一检查不过 → 构建失败（能否收到通知取决于你的 GitHub 账号通知设置，本仓库未内置告警），同时 `build-info.json` 里带上游 commit SHA，便于回溯定位是哪次上游变更引起。
