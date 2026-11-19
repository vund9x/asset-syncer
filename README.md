# asset-syncer

Bộ đồng bộ và cập nhật tài nguyên (assets/files) đa năng viết bằng Go, hoạt động theo cơ chế khai báo (declarative configuration). Chương trình đọc danh sách nguồn từ file cấu hình JSON (`sources.json`), tải dữ liệu từ internet (URL trực tiếp, GitHub Repo, GitHub Releases), cập nhật image Docker/Podman từ `docker-compose.yml`, và **chỉ ghi đè khi nội dung thực sự thay đổi** (so sánh mã băm SHA-256 / image ID).

> **Ghi chú lịch sử:** Ban đầu, dự án này được viết riêng để làm trình cập nhật scripts & shaders cho trình phát video MPV (`mpv-updater`). Tuy nhiên, nhờ kiến trúc thiết kế tổng quát (cho phép tải file, giải nén zip, lọc theo glob pattern...), công cụ này hiện có thể dùng để đồng bộ bất kỳ loại file hoặc thư mục tài nguyên nào.

## Cấu trúc thư mục ví dụ

Khi sử dụng làm trình đồng bộ cho MPV và các công cụ liên quan, cấu trúc thư mục của bạn có thể trông như sau:

```
mpv/
├─ portable_config/         # Thư mục đích nhận tài nguyên (cấu hình trong root)
│  ├─ scripts/
│  └─ shaders/
└─ asset-syncer/            # Thư mục chứa mã nguồn và cấu hình của công cụ này
   ├─ asset-syncer.exe      # File thực thi sau khi build (hoặc asset-syncer trên Linux/macOS)
   ├─ main.go
   ├─ go.mod
   ├─ sources.json          # File cấu hình gốc (root config)
   ├─ sources.d/            # Thư mục chứa các cấu hình con chia theo nhóm
   │  ├─ scripts.json       # Danh sách scripts của MPV
   │  ├─ shaders.json       # Danh sách shaders của MPV
   │  └─ streamlink.json    # Cấu hình Streamlink
```

`sources.json` (root) trông như:

```json
{
  "root": "../portable_config",
  "include": [
    "sources.d/*.json"
  ]
}
```

- `include` là mảng glob (đường dẫn **tương đối** so với file config chứa nó).
- File con có schema giống file root nhưng **chỉ field `sources`** được dùng;
  `root` ở file con bị bỏ qua.
- File con cũng có thể có `include` riêng → load đệ quy. Cycle được tự xử lý.
- `name` của mỗi source phải **duy nhất** trên toàn bộ tập config.
- Bạn có thể tạo thêm `sources.d/my-extras.json` (hoặc bất kỳ tên nào) để
  thêm source riêng — không cần đụng vào `sources.json` hay các file mặc định.

## Cài đặt & Sử dụng

### 1. Tải bản dựng sẵn (Khuyên dùng)
Bạn có thể tải các bản build sẵn cho Windows, Linux, và macOS tại mục **[Releases](https://github.com/vund9x/asset-syncer/releases)**. Hãy tải về file thực thi phù hợp với hệ điều hành và kiến trúc máy của bạn (ví dụ: `asset-syncer-windows-amd64.exe` cho Windows 64-bit), đặt vào thư mục chạy và bắt đầu cấu hình.

### 2. Tự build từ mã nguồn (Yêu cầu Go 1.21+)

```powershell
go build -o asset-syncer.exe
.\asset-syncer.exe                 # cập nhật tất cả
.\asset-syncer.exe -dry-run        # xem trước, không ghi
.\asset-syncer.exe -only uosc,memo # chỉ cập nhật một số
.\asset-syncer.exe -v              # verbose
.\asset-syncer.exe -jobs 8         # tải song song
.\asset-syncer.exe -pause always   # luôn chờ Enter trước khi thoát
```

- `-pause` (`auto` mặc định | `always` | `never`): `auto` chỉ hiện "Nhấn
  Enter để thoát..." khi bạn **double-click** file exe trên Windows (console
  được tạo riêng cho tiến trình). Chạy từ terminal, từ cron/systemd hoặc trên
  Linux/macOS sẽ không bị chặn. `always` ép luôn chờ Enter, `never` tắt hẳn.
- Output tự tô màu khi chạy trên terminal: `OK` xanh lá, `UPDATED` cyan,
  `FAIL` đỏ. Khi redirect/piped (log, cron) màu tự tắt; đặt `NO_COLOR=1`
  để tắt màu thủ công.

Hoặc chạy trực tiếp không cần build:

```powershell
go run . -dry-run
```

## sources.json

Mỗi entry thuộc một trong các loại sau:

### `file` — tải 1 file đơn lẻ

```json
{
  "name": "thumbfast",
  "type": "file",
  "url":  "https://raw.githubusercontent.com/po5/thumbfast/master/thumbfast.lua",
  "dest": "scripts/thumbfast.lua"
}
```

### Giải nén archive (`extract`)

Cả `file` lẫn `release` đều có thể tự giải nén `.zip` sau khi tải:

```json
{
  "name": "streamlink",
  "type": "release",
  "repo": "streamlink/windows-builds",
  "tag":  "latest",
  "asset": "streamlink-*-py*-x86_64.zip",
  "extract": true,
  "strip_components": 1,
  "dest": "../streamlink"
}
```

- `extract: true` → coi nội dung tải về là zip, giải nén vào `dest` (lúc này
  `dest` được xem là **thư mục**).
- `strip_components: N` → bỏ N thành phần đầu trong path (giống `tar
  --strip-components`). Streamlink zip có top-dir `streamlink-<ver>-...-x86_64/`
  nên `1` là đủ. Phiên bản đổi không sao vì component đầu luôn bị strip.
- `path_prefix` (tuỳ chọn) → chỉ lấy file dưới một đường dẫn nhất định *bên
  trong zip*; áp dụng **trước** `strip_components`.
- `include` / `exclude` (glob) → lọc trên path tương đối *sau khi* đã strip.
- `dest` có thể là **đường dẫn tuyệt đối** (`"D:/PortableApps/streamlink"`)
  hoặc tương đối; đường dẫn tương đối được join với `root`. Tuyệt đối sẽ
  bỏ qua `root` — hữu ích để cài thứ ngoài `portable_config`.

> Lưu ý: tool chỉ **thêm/ghi đè**. File cũ không còn trong zip (vd. wheel
> bản cũ) sẽ **không** bị xoá tự động. Khi muốn cài "sạch" bản mới, xoá
> thư mục `dest` rồi chạy lại.

### `release` — tải asset từ GitHub Releases

```json
{
  "name": "shader-FSRCNNX_x2_16",
  "type": "release",
  "repo": "igv/FSRCNN-TensorFlow",
  "tag":  "latest",
  "asset": "FSRCNNX_x2_16-*.glsl",
  "dest": "shaders/FSRCNNX"
}
```

- `tag`: `"latest"` (mặc định) hoặc một tag cụ thể như `"1.1"`.
- `asset`: glob khớp với tên file asset. Nếu khớp nhiều file → tất cả được
  tải vào thư mục `dest`. Nếu khớp đúng 1 và `dest` có đuôi file → ghi đúng
  vào path đó.
- Tool đọc qua **GitHub REST API** (`/repos/<owner>/<repo>/releases/latest`).
  GitHub anonymous limit là 60 req/giờ. Nếu hay bị rate-limit, set biến môi
  trường `GITHUB_TOKEN` (Personal Access Token, không cần scope nào):

  ```powershell
  $env:GITHUB_TOKEN = "ghp_xxx"
  .\asset-syncer.exe
  ```

> Khác với `type: "file"` (dùng URL `releases/download/<TAG>/...` cố định),
> `type: "release"` luôn theo bản **mới nhất** mà tác giả publish.

### `repo` — tải zip một repo GitHub, copy 1 thư mục con

```json
{
  "name": "uosc",
  "type": "repo",
  "repo": "tomasklaen/uosc",
  "branch": "main",
  "path_prefix": "src/uosc",
  "dest": "scripts/uosc",
  "include": ["*.lua", "**/*.lua"],
  "exclude": [".gitignore"]
}
```

- `path_prefix`: thư mục bên trong repo cần lấy (rỗng = lấy toàn repo).
- `include` / `exclude`: glob pattern (so với path tương đối bên trong `dest`).
  Nếu `include` rỗng = lấy tất.
- `branch`: nếu để `master` mà repo dùng `main` thì tool tự thử lại với `main`.
- `disabled: true` để tạm tắt một entry mà vẫn giữ nguyên cấu hình. Bỏ trường
  này hoặc đổi thành `false` để bật lại. Ví dụ:

  ```json
  {
    "name": "shader-ACNet",
    "type": "repo",
    "repo": "TianZerL/ACNetGLSL",
    "dest": "shaders/ACNet",
    "disabled": true
  }
  ```

- Source bị tắt sẽ được bỏ qua khi chạy cập nhật, kể cả khi tên của nó được
  truyền qua `-only`.

### `compose` — cập nhật image Docker/Podman từ docker-compose.yml

Pull image mới cho các service trong một file compose, rồi **chỉ recreate**
những container có image thực sự thay đổi (giống triết lý "chỉ ghi khi khác"
của các type khác). Tool **không sửa** file compose: service pin tag cố định
(vd. `nginx:1.25.3`) sẽ đứng yên; chỉ các tag "động" như `:latest` mới trôi
theo registry.

```json
{
  "name": "homelab-compose",
  "type": "compose",
  "file": "/srv/docker/docker-compose.yml",
  "services": ["nginx", "grafana"],
  "engine": "auto",
  "prune": true,
  "force_recreate": true
}
```

- `file` (bắt buộc): đường dẫn tới file compose — tuyệt đối, hoặc tương đối
  theo `root`. Nên dùng đường dẫn tuyệt đối khi chạy trên homelab.
- `services` (tuỳ chọn): chỉ update một số service. Bỏ trống = tất cả service
  có khai báo `image:`. Service chỉ có `build:` bị bỏ qua.
- `engine` (tuỳ chọn): `auto` (mặc định), `docker`, hoặc `podman`. Với `auto`,
  tool thử lần lượt `docker compose` → `podman compose` → `podman-compose`.
- `compose_cmd` (tuỳ chọn): ép dùng một lệnh khác, vd. `"podman-compose"`,
  `"docker-compose"` (v1) hoặc đường dẫn tuyệt đối. Khi dùng trường này, tool
  suy ra engine từ tên lệnh (chứa `docker`/`podman`); nếu không suy ra được
  thì phải set `engine` thủ công.
- `prune` (tuỳ chọn, mặc định `false`): sau khi có image được pull/cập nhật
  thật sự, chạy thêm `<engine> image prune -a -f` để dọn image cũ không còn
  dùng (`docker image prune -a -f` hoặc `podman image prune -a -f`). Không
  chạy khi mọi thứ đã up-to-date.
  **Lưu ý:** cờ `-a` xoá **toàn bộ** image không được container nào sử dụng
  trên máy — kể cả image của stack khác đang tắt. Chỉ bật nếu bạn chấp nhận
  điều này.
- `force_recreate` (tuỳ chọn, mặc định `false`): thêm `--force-recreate` vào
  lệnh `up -d`. Cần thiết với một số phiên bản `podman-compose` không tự
  recreate container khi tag giữ nguyên mà chỉ digest đổi (vd. `:latest`
  được push bản mới). Chỉ áp dụng cho các service có image thay đổi.

Cách hoạt động:

1. Đọc mục `services:` trong file compose (hỗ trợ YAML anchors, `x-`
   extensions; service không có `image:` bị bỏ qua).
2. Ghi nhận image ID hiện có (`docker/podman image inspect`).
3. Chạy `compose pull` cho các service đã chọn.
4. So sánh image ID trước/sau: chỉ service có image đổi (hoặc image chưa có
   trên máy) mới được đưa vào `compose up -d`.
5. Nếu bật `prune`: chạy `<engine> image prune -a -f` (chỉ khi bước 4 có
   thay đổi).
6. Báo cáo `[added]` / `[updated]` / unchanged như các type khác.

Ví dụ output trên homelab:

```
  [updated]    nginx                nginx:latest (6efc10a1... -> 8a2c9f31...)
[UPDATED] homelab-compose         changed=1 added=0 unchanged=1
```

Lưu ý:

- Tool gọi CLI docker/podman **cùng máy** với nó. Nếu engine cần quyền root
  (docker socket), chạy tool bằng user phù hợp (hoặc `sudo -E ./asset-syncer`).
- `-dry-run` chỉ in danh sách service/image sẽ pull, **không** cần cài
  docker/podman — hữu ích để validate config từ máy khác.
- Có thể chạy định kỳ bằng cron/systemd. Với `-pause auto` (mặc định), tool
  chỉ hiện "Nhấn Enter để thoát..." khi được double-click trên Windows, nên
  job không bị block:

  ```bash
  /srv/bin/asset-syncer -config /srv/bin/sources.json -only homelab-compose
  ```

## Ghi chú

- Đường dẫn `root` trong `sources.json` mặc định là `../portable_config` (khi chạy từ thư mục `asset-syncer/`, thư mục đích sẽ nằm song song với thư mục repo). Có thể đổi nếu cần.
- Một số shader (RAVU/NNEDI3, hdeband, adaptive-sharpen) đang bị `disabled`
  vì cần chọn variant cụ thể — bật lên và chỉnh `include` cho phù hợp.
- File ngoài danh sách (script bạn tự viết) sẽ **không bị xoá**, tool chỉ
  thêm/ghi đè theo `sources.json`.
