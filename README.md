# Nube — یک ارکستریتور کانتینر شبیه Kubernetes، از صفر

Nube (به اسپانیایی یعنی «ابر») یک ارکستریتور کانتینر کوچک ولی واقعی است که با Go نوشته شده.
یک باینری (`nube`) هم control plane است، هم agent نودها و هم CLI. یک داشبورد وب هم داخلش embed شده.

```
                 ┌──────────── control plane (۱ یا ۳ سرور، Raft، HTTPS) ────────────┐
   nube CLI ───► │ REST API · RBAC · scheduler · controllers · LB · Ingress · UI   │
   مرورگر   ───► │ state store (فایل JSON یا Raft تکثیرشده) · cluster CA          │
                 └──────────────▲──────────────────────────────▲──────────────────┘
                   sync (HTTPS + node token)        sync (HTTPS + node token)
                 ┌──────────────┴─────────┐   ┌────────────────┴───────┐
                 │ node agent  (Docker)   │   │ node agent  (Docker)   │
                 │ pods 10.88.1.0/24      │◄─►│ pods 10.88.2.0/24      │
                 │ DNS + service proxy    │ WireGuard   DNS + proxy    │
                 └────────────────────────┘   └────────────────────────┘
```

## قابلیت‌ها

| حوزه | چه چیزی پیاده شده |
|---|---|
| **Pod** | چند کانتینر در هر pod که `localhost` مشترک دارند (مثل Kubernetes، با یک کانتینر pause که صاحب شبکه است)، init container که به ترتیب اجرا می‌شود، و `emptyDir` مشترک (روی دیسک یا حافظه، با محدودیت حجم) |
| **Workloads** | `Deployment` با rolling update، rollback و autoscale. `StatefulSet` با نام‌های پایدار (`db-0`، `db-1`)، شروع مرتب، یک volume برای هر replica و DNS برای هر pod. `Job` با completions، parallelism، backoffLimit و deadline. `CronJob` با زمان‌بندی cron و time zone و concurrencyPolicy |
| **Scheduler** | فیلتر بر اساس Ready/cordon، `nodeSelector`، CPU/RAM درخواستی، host port و محل volume. پخش replicaها روی نودهای مختلف |
| **Self-healing** | restart با backoff نمایی برای هر کانتینر، health check از نوع HTTP و TCP، جابه‌جایی podها وقتی نودی از دست می‌رود |
| **Networking** | subnet جداگانه برای هر نود، overlay رمزنگاری‌شده با **WireGuard** (یا host-gw)، ارتباط مستقیم pod به pod |
| **Services** | **TCP و UDP**، پراکسی روی هر نود (شبیه ClusterIP)، سرویس‌های `headless` (DNS مستقیم به IP podها) و `internal` (فقط داخل کلاستر)، load balancer روی سرورها |
| **Ingress** | مسیریابی HTTP/HTTPS بر اساس hostname (شامل `*.example.com`) و path روی پورت‌های 80/443، با TLS از یک Secret نوع `tls` یا گواهی self-signed، و redirect از HTTP به HTTPS |
| **DNS** | `web`، `web.prod`، `web.default.svc.nube`، `db-0.db` و `POD.default.pod.nube` |
| **Storage** | `Volume` از نوع `local` با **حجم اجباری** (هر volume یک فایل‌سیستم ext4 مستقل است و بیشتر از اندازه‌اش نمی‌شود نوشت)، `nfs` و `hostPath`. گزارش مصرف واقعی هر volume |
| **امنیت** | **API روی HTTPS** با CA اختصاصی کلاستر (کلاینت‌ها hash آن را pin می‌کنند)، **mutual TLS** بین سرور و agentها، **توکن‌های چندگانه با نقش** (`admin`، `edit`، `view`، `node`) و محدودسازی به namespace، و ماسک‌شدن مقدار secretها |
| **High availability** | ۳ (یا ۵) سرور با **Raft**. در صورت از کار افتادن leader، ظرف چند ثانیه leader جدید انتخاب می‌شود. agentها خودکار به سرور سالم سوییچ می‌کنند |
| **Observability** | داشبورد زنده، رویدادها، لاگ زنده‌ی هر کانتینر (`logs -c`)، مصرف CPU، RAM و دیسک |

### چه چیزهایی واقعاً تست شده
روی یک کلاستر Docker با ۳ سرور HA (با TLS) و ۲ نود (Docker-in-Docker با WireGuard)، و یک نصب تمیز روی Ubuntu 24.04:

- **Pod چندکانتینری:** init container صفحه را نوشت، nginx سرو کرد، و sidecar از طریق `localhost` به آن رسید.
- **StatefulSet:** `db-1` فقط بعد از Ready شدن `db-0` بالا آمد. هر replica volume خودش را داشت. `db-0.db` به pod درست resolve شد. rolling restart از بالاترین ordinal انجام شد.
- **Volume:** نوشتن ۱۰۰MB در یک volume با حجم ۶۴Mi با خطای «No space left on device» متوقف شد.
- **Job و CronJob:** Job دقیقاً ۳ completion را دوتا‌دوتا اجرا کرد. CronJob هر دقیقه اجرا شد و history limit رعایت شد.
- **UDP:** سرویس UDP هم داخل کلاستر و هم از طریق load balancer کار کرد.
- **Ingress:** مسیریابی با host و path، و HTTPS با گواهی همان Secret.
- **امنیت:** توکن view نتوانست تغییری بدهد، توکن edit محدود به `dev` نتوانست `default` را ببیند، توکن revoke‌شده فوراً از کار افتاد، و CA اشتباه رد شد.
- **HA:** با kill کردن leader، leader جدید انتخاب شد و درخواست‌ها روی HTTPS به آن فرستاده شد.

---

## نصب روی Ubuntu

### ۱) ساخت (روی ویندوز)
```powershell
powershell -ExecutionPolicy Bypass -File .\build.ps1
```
خروجی: `dist\nube-0.2.0-linux.tar.gz` (باینری amd64 و arm64، اسکریپت نصب، systemd و مثال‌ها) و `dist\nube.exe` برای CLI روی ویندوز.

### ۲) همه‌چیز روی یک ماشین
```bash
scp dist/nube-0.2.0-linux.tar.gz user@SERVER:~
ssh user@SERVER
tar xzf nube-0.2.0-linux.tar.gz && cd nube-0.2.0-linux
sudo bash install.sh single
```
اسکریپت این کارها را انجام می‌دهد:
- نصب Docker ‏(`docker.io` از مخزن Ubuntu)، WireGuard، iptables، nfs-common و e2fsprogs
- ساخت CA کلاستر و توکن admin، و ساخت یک توکن `node` برای agent
- نصب سرویس‌های systemd و ذخیره‌ی تنظیمات CLI برای کاربر شما

آخر کار آدرس داشبورد (`https://SERVER_IP:7070`)، hash مربوط به CA و دستور اضافه کردن نود را چاپ می‌کند.
برای حذف هشدار مرورگر، `https://SERVER_IP:7070/ca.crt` را دانلود و به‌عنوان CA قابل‌اعتماد import کنید.

```bash
nube get nodes
nube apply -f examples/web.yaml
curl http://SERVER_IP:8080
```

### ۳) اضافه کردن نود
همان دستوری که اسکریپت نصب چاپ کرده را روی ماشین دوم اجرا کنید:
```bash
sudo bash install.sh agent --server https://SERVER_IP:7070 --token nube_xxx --ca-hash sha256:...
```
توکن نود فقط اجازه‌ی ثبت نود و همگام‌سازی podها را دارد. توکن admin هیچ‌وقت روی نودها نمی‌رود.

### ۴) control plane با HA ‏(۳ سرور)
```bash
# سرور ۱
sudo bash install.sh server --ha --id s1 --bootstrap
# سرور ۲ و ۳: توکن admin از /var/lib/nube/token و hash از /var/lib/nube/ca.hash سرور ۱
sudo bash install.sh server --ha --id s2 --join https://S1_IP:7070 --token ADMIN_TOKEN --ca-hash sha256:...
sudo bash install.sh server --ha --id s3 --join https://S1_IP:7070 --token ADMIN_TOKEN --ca-hash sha256:...
```
اگر می‌خواهید سرورها خودشان هم pod اجرا کنند، به‌جای `server` از `single` استفاده کنید. کلاستر ۳تایی از دست رفتن یک سرور را تحمل می‌کند.

### پورت‌ها
| پورت | کاربرد |
|---|---|
| 7070/tcp | API و داشبورد (HTTPS) |
| 7071/tcp | API ‏agent (mutual TLS، فقط سرورها) |
| 7072/tcp | Raft (فقط بین سرورها) |
| 51820/udp | WireGuard (بین نودها) |
| 80, 443/tcp | Ingress |
| پورت سرویس‌ها | load balancer روی سرورها |

حذف: `sudo bash install.sh uninstall` (داده‌ها و کانتینرهای در حال اجرا حذف نمی‌شوند).

---

## CLI

```bash
nube login --server https://SERVER:7070 --token TOKEN      # fingerprint مربوط به CA را نشان می‌دهد و می‌پرسد
nube apply -f app.yaml
nube get nodes|deploy|sts|jobs|cronjobs|pods|svc|ing|cm|secrets|volumes [-n NS | -A] [-o wide|yaml] [-w]
nube describe pod NAME
nube logs POD [-c CONTAINER] [-f]
nube scale deploy|sts NAME --replicas 5
nube set image deploy NAME nginx:1.27 [-c CONTAINER]
nube rollout status|history|undo|restart deploy|sts NAME
nube trigger cronjob NAME          # اجرای فوری
nube suspend|resume cronjob NAME
nube token create ci --role edit -n prod       # فقط یک بار نشان داده می‌شود
nube token list | nube token delete ci
nube whoami
nube top nodes|pods
nube cordon|uncordon|drain NODE
nube cluster status
```

### نقش‌ها
| نقش | اجازه |
|---|---|
| `admin` | همه‌چیز: نودها، توکن‌ها و HA |
| `edit` | خواندن و تغییر workloadها، configها و secretها (اگر namespace داده شود، فقط در همان namespaceها) |
| `view` | فقط خواندن (بدون دیدن مقدار secretها) |
| `node` | فقط برای agentها: ثبت نود و همگام‌سازی |

---

## مرجع manifest (خلاصه)

```yaml
kind: Deployment            # یا StatefulSet / Job / Pod
metadata: { name: app, namespace: default }
spec:
  replicas: 3
  template:
    # یک کانتینر: فیلدها مستقیم اینجا (image, command, ports, ...)
    # چند کانتینر:
    emptyDirs: [{ name: cache, sizeLimit: 256Mi, medium: Memory }]
    initContainers:
      - { name: migrate, image: myapp, command: ["./migrate"] }
    containers:
      - name: app
        image: myapp:1.2
        ports: [{ containerPort: 8080 }, { containerPort: 9000, protocol: UDP }]
        env: { KEY: value }
        envFrom: [{ configMap: app-config }, { secret: app-secret }]
        resources: { cpu: 250m, memory: 256Mi }
        healthCheck: { httpGet: { path: /healthz, port: 8080 } }
        volumes:
          - { volume: data, containerPath: /data }     # Volume کلاستر
          - { emptyDir: cache, containerPath: /cache }  # مشترک بین کانتینرهای pod
      - name: sidecar
        image: busybox
    restartPolicy: Always    # Always | OnFailure | Never
---
kind: Service
metadata: { name: app }
spec:
  ports: [{ port: 8080 }, { port: 9000, protocol: UDP }]
  # headless: true  → DNS مستقیم به IP podها (برای StatefulSet)
  # internal: true  → فقط داخل کلاستر
---
kind: Volume
metadata: { name: data }
spec: { type: local, size: 10Gi }    # حجم اجباری
```

نمونه‌های کامل در `examples/`:

| فایل | محتوا |
|---|---|
| `web.yaml` | وب‌سرور ساده با Service |
| `redis.yaml` | Redis با volume |
| `postgres.yaml` | Postgres با Secret و volume |
| `api-autoscale.yaml` | API با autoscale و ConfigMap |
| `sidecar.yaml` | init container و sidecar |
| `statefulset.yaml` | Postgres در قالب StatefulSet |
| `jobs.yaml` | Job و CronJob |
| `ingress.yaml` | Ingress با host و path |
| `udp.yaml` | سرویس UDP |
| `nfs-volume.yaml` | volume از نوع NFS |

---

## چطور کار می‌کند

- **State و reconciliation:** کنترلرها هر ثانیه روی leader اجرا می‌شوند و وضعیت واقعی را به وضعیت مطلوب می‌رسانند.
  کد: `internal/server/controllers.go` و `workloads.go`.
- **Pod:** agent برای هر pod یک کانتینر pause می‌سازد که IP، پورت‌ها و DNS را نگه می‌دارد. بعد init containerها را به ترتیب
  اجرا می‌کند و کانتینرهای اصلی را با `--network container:<pause>` به همان شبکه وصل می‌کند. image مربوط به pause از خود
  باینری nube ساخته می‌شود و نیازی به registry ندارد. کد: `internal/agent/pod.go`.
- **Volume با حجم ثابت:** هر volume یک فایل image با ext4 است که به‌صورت loop mount می‌شود. افزایش `size` باعث بزرگ‌شدن
  آنلاین آن می‌شود و کوچک‌کردن مجاز نیست. کد: `internal/agent/volumes.go`.
- **امنیت:** اولین leader یک CA می‌سازد که داخل state تکثیرشده نگه داشته می‌شود. هر سرور با آن برای خودش گواهی صادر می‌کند.
  agentها CSR می‌فرستند و گواهی‌شان را با IP خودشان می‌گیرند. سرور با client certificate به agentها وصل می‌شود.
  کد: `internal/pki` و `internal/server/auth.go`.
- **HA:** هر تغییر state یک entry در لاگ Raft است. فقط leader کنترلرها را اجرا می‌کند. کد: `internal/store/raft.go`.

## محدودیت‌هایی که هنوز هست

- **DaemonSet**، **NetworkPolicy**، CRD و operator وجود ندارد.
- Ingress گواهی Let's Encrypt را خودکار نمی‌گیرد. گواهی را (مثلاً با certbot) در یک Secret نوع `tls` بگذارید.
- volume از نوع `local` به نودش وابسته است. حجم volumeهای `nfs` را سرور NFS مدیریت می‌کند. snapshot و backup خودکار ندارد.
- runtime فقط Docker است.
- در HA کل state در هر entry تکثیر می‌شود. این برای ده‌ها نود و صدها pod کافی است، نه برای هزاران.
- گواهی‌ها self-signed هستند (CA خود کلاستر). برای دسترسی از اینترنت، داشبورد را پشت Ingress با گواهی معتبر بگذارید.

## توسعه و تست

```bash
go test ./...          # scheduler، rolling update، Job، CronJob، StatefulSet، RBAC، Ingress، UDP proxy، PKI و ...
make linux             # یا powershell -ExecutionPolicy Bypass -File .\build.ps1
bash test/e2e/up.sh    # کلاستر تستی روی Docker: ۳ سرور HA با TLS و ۲ نود
```

## عیب‌یابی

| مشکل | راه‌حل |
|---|---|
| `server certificate is not signed by the pinned cluster CA` | hash را با `/var/lib/nube/ca.hash` روی سرور مقایسه کنید |
| نود `NotReady` است | `journalctl -u nube-agent -n 50`. آیا agent به پورت 7070 دسترسی دارد و توکن از نوع `node` است؟ |
| ستون NETWORK مقدار `wireguard(down)` دارد | خطا در صفحه‌ی Nodes دیده می‌شود. معمولاً `wireguard-tools` نصب نیست یا UDP 51820 بسته است |
| در وضعیت volume پیام `size not enforced` هست | `e2fsprogs` روی نود نیست، یا volume قبل از تعیین size ساخته شده |
| pod در `Pending` یا `Init` مانده | `nube describe pod NAME` و `nube logs POD -c INIT_CONTAINER` |
| `403 ... needs role edit` | توکن شما نقش کافی ندارد (`nube whoami`) |
