# geosite

构建 `geosite.dat`：以 [v2fly/domain-list-community](https://github.com/v2fly/domain-list-community) 的**最新数据**为基底，依据 [Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat) 的增强思路，叠加**中国域名**、**被墙域名（GFWList）**与**广告域名**，去重后打包。

最终产物只保留 `cn`、`geolocation-!cn`、`category-ads-all` 三个分类；GFWList 的域名已并入 `geolocation-!cn`，不再单独提供 `gfwlist` 分类。每个分类在打包前都已完全展开，产物中不再存在 `include:` 关系。

## 与官方 `dlc.dat` 的区别

- `cn` 清单额外并入中国域名（`felixonmars/dnsmasq-china-list` 的 `accelerated-domains.china.conf`）。
- `category-ads-all` 清单额外并入 4 份广告域名源（EasyList China + EasyList、AdGuard DNS filter、pgl.yoyo.org、someonewhocares）。
- `gfwlist` 的域名并入 `geolocation-!cn`，不再单独提供 `GFWLIST` 分类。
- 打包前把 `cn`、`geolocation-!cn`、`category-ads-all` 完全展开，删除其余上游分类；最终 dat 只含这三个 section。
- **不生成** `china-list` 等重复分段。

## 冲突策略

`cn` 的候选域名会与**展开后的 `geolocation-!cn`**（基底代理表 + `gfwlist`）比对，命中即**不并入 `cn`**，保留在代理表 —— 即「代理优先」。

- `gfwlist` 在并入 `geolocation-!cn` 前，先与基底 `cn`、基底 `geolocation-!cn` 去重（**基底 `cn` 优先**：冲突的从 `gfwlist` 剔除）；
- 因 `geolocation-!cn` 需要并入 `gfwlist`，所以**必须先完成 `gfwlist` 的生成，再处理 `cn`**。

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
   减掉与基底 `cn`、基底 `geolocation-!cn` 冲突的域名，得到本次保留的 gfwlist 域名；
3. 再处理 `cn`：`cn` 的排除集是展开后的 `geolocation-!cn`（基底 `!cn` ∪ gfwlist），然后按域名语义比对：
   - 域名 `x` 被根 `r` 覆盖 ⟺ `x == r` 或 `x` 以 `.r` 结尾（`domain:` 语义）；
   - `full:` 仅精确匹配；
   只把「基底没有、且不与展开后的 `geolocation-!cn` 冲突」的域名并入 `cn`；
4. 广告源并入 `category-ads-all`；
5. 打包前裁剪：用基底平铺清单 + 本次新增域名生成 3 份最终清单，
   清空 `data` 后只写回 `cn` / `geolocation-!cn` / `category-ads-all`，其余分类全部删除；
6. 最后交给上游构建器打包，上游会再做一次父子去重。

## 目录结构

```
.
├─ .github/workflows/build.yml   构建流程（手动触发 + 构建相关文件变更时自动触发）
├─ cmd/merge/main.go             展开比对、去重、冲突过滤、合并
├─ cmd/merge/finalize.go         打包前展开裁剪，只保留三个最终分类
├─ cmd/verify/main.go            产物体检（自带的轻量 protobuf 解析）
├─ lookup/main.go                按域名反查命中的分类（独立模块的本地排障小工具）
└─ config/sources.json           外部源定义（URL / 格式 / 目标清单 / includes / final_lists / verify_sources）
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

默认情况下，`cmd/merge` 会在合并后自动展开裁剪：`community/data` 最终只剩 `cn`、`geolocation-!cn`、`category-ads-all` 三个平铺文件，产物只包含这三个 section。体检用的中间文件写在 `base/verify/` 下，体检会自动读取。

> 重复本地构建时，请先重新 clone 上游，或在 `community` 里执行 `git checkout -- data` 恢复原始数据；否则下一次“导出基底”会基于已裁剪的三个文件，结果不再完整。

## 使用

```
geosite:cn
geosite:geolocation-!cn
geosite:category-ads-all
```

`gfwlist` 的域名已并入 `geolocation-!cn`，不再提供独立的 `geosite:gfwlist`。

产物同时提供 `geosite.dat.sha256sum`、`build-info.json`（记录上游 commit、各源统计与最终分类）、`verify.json`。

每次发布后只保留**最近 5 个** release（含 tag 与附件），更早的版本会自动清理；需要长期留存的版本请自行备份。

## 定时更新（可选）

本仓库**不使用** GitHub Actions 自带的 `schedule` 定时器：其触发延迟严重，**实际观测延迟可达数小时**，高峰期还可能被跳过。

如需每日自动更新，建议使用外部调度器（服务器 `crontab`、cron-job.org 等）调用 GitHub API 的 `workflow_dispatch` 触发本 workflow。Token 使用仅授予 **Actions: write** 权限的 fine-grained PAT，只保存在调度器侧，切勿提交到仓库。

## 按域名反查（lookup）

`lookup/` 是一个独立小工具（自带模块、零第三方依赖），按 v2ray/xray 的匹配语义反查某个域名命中了哪些分类，便于判断该走代理还是直连：

```bash
cd lookup && go build -o lookup .
./lookup -dat ../publish/geosite.dat -v www.google.com
```

输出示例：

```
www.google.com
  - GEOLOCATION-!CN          domain:google.com
```

`-json` 输出结构化结果，`-v` 显示命中的具体规则；不传域名参数时从标准输入逐行读取。详见 `lookup/README.md`。

## 体检与上游同步

每次构建会把上游最新 commit 拉下来，构建后自动体检：

- section 集合必须正好是 `CN` / `GEOLOCATION-!CN` / `CATEGORY-ADS-ALL`；
- `cn` / `category-ads-all` 条目数下限，以及 gfwlist 源条目数下限；
- **`geolocation-!cn` = 基底 + `gfwlist`**：无越界域名泄漏，且 `gfwlist` 全部并入；
- 产物体积区间。

任一检查不过 → 构建失败（能否收到通知取决于你的 GitHub 账号通知设置，本仓库未内置告警），同时 `build-info.json` 里带上游 commit SHA，便于回溯定位是哪次上游变更引起。
