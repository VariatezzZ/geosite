# lookup

按域名反查 `geosite.dat`：输入一个域名，输出它命中了**哪些分类**、以及是哪条规则命中的。

匹配语义与 V2Ray/Xray 路由完全一致：

| 类型 | 语义 |
|---|---|
| `keyword` | 子串包含 |
| `regex` | 正则匹配 |
| `domain` | 该域名本身及其所有子域 |
| `full` | 仅精确相等 |

复用仓库 `internal/geosite` 的共享解码器, 零第三方依赖. 编译后的二进制可单独复制运行; 源码需连同根模块一起编译.

## 编译

```bash
cd lookup
go build -o lookup .
```

`lookup` 属于仓库根模块, 没有自己的 `go.mod`; 在仓库内执行上述命令即可.

## 用法

```bash
# 默认读取「与本程序同级目录」下的 geosite.dat
./lookup www.google.com

# 指定 dat 路径（例如 Xray 的）
./lookup -dat /opt/Xray/geosite.dat www.google.com

# 显示命中的具体规则，便于排障
./lookup -v ads.example.com

# 多个域名 / 管道批量，输出 JSON
./lookup -dat /opt/Xray/geosite.dat --json a.com b.cn
printf 'a.com\nb.cn\n' | ./lookup --json
```

不传域名参数时，从标准输入逐行读取。

### 参数

| 参数 | 说明 |
|---|---|
| `-dat` | `geosite.dat` 路径，默认与本程序同级 |
| `-json` | 以 JSON 输出（含 `domain` / `categories` / `matches`） |
| `-v` | 文本模式下列出命中规则 `type:value` |

### 输出示例

```
$ ./lookup -v www.google.com
www.google.com
  - geolocation-!cn         domain:google.com
```

```json
[
  {
    "domain": "www.google.com",
    "categories": ["geolocation-!cn"],
    "matches": [
      { "category": "geolocation-!cn", "type": "domain", "value": "google.com" }
    ]
  }
]
```

## 说明

- 输入会自动去空格、转小写、剥离 `scheme://`、路径与末尾的 `.`，所以直接粘 URL 也能用。
- 采用线性扫描，单次查询在中低端机器上为百毫秒级；用于排障足够，不适合当作高频服务的内核。
- 解析器复用仓库 `internal/geosite` 的共享实现, 以保持零第三方依赖; 编译后的二进制可独立搬运.
