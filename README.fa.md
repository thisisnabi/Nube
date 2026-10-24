# Nube — یک ارکستریتور کانتینر شبیه Kubernetes، از صفر

Nube (به اسپانیایی یعنی «ابر») یک ارکستریتور کانتینر کوچک ولی واقعی است که با Go نوشته شده.
یک باینری (`nube`) هم control plane است، هم agent نودها و هم CLI. یک داشبورد وب هم داخلش embed شده.
کانتینرها روی Docker یا Podman اجرا می‌شوند. Nube **agentهای هوش مصنوعی** (Claude یا هر مدل سازگار با OpenAI) را هم مثل
یک workload اجرا می‌کند، و agentها می‌توانند از طریق MCP به‌صورت امن **خود کلاستر را مدیریت کنند**.

[English](README.md)

```
                 ┌──────────── control plane (۱ یا ۳ سرور، Raft، HTTPS) ────────────┐
   nube CLI ───► │ REST API · RBAC · scheduler · controllers · LB · Ingress · UI   │
   مرورگر   ───► │ state store (فایل JSON یا Raft تکثیرشده) · cluster CA          │
                 └──────────────▲──────────────────────────────▲──────────────────┘
                   sync (HTTPS + node token)        sync (HTTPS + node token)
                 ┌──────────────┴─────────┐   ┌────────────────┴───────┐
                 │ node agent  (Docker)   │   │ node agent  (Podman)   │
                 │ pods 10.244.1.0/24     │◄─►│ pods 10.244.2.0/24     │
                 │ DNS + service proxy    │ WireGuard   DNS + proxy    │
                 └────────────────────────┘   └────────────────────────┘
```

## قابلیت‌ها

| حوزه | چه چیزی پیاده شده |
|---|---|
| **Pod** | چند کانتینر در هر pod که `localhost` مشترک دارند (مثل Kubernetes، با یک کانتینر pause که صاحب شبکه است)، init container که به ترتیب اجرا می‌شود، و `emptyDir` مشترک (روی دیسک یا حافظه، با محدودیت حجم) |
| **Workloads** | `Deployment` با rolling update، **rollback خودکار rolloutهای ناموفق** و autoscale. **با تغییر ConfigMap یا Secret، podها خودکار rolling restart می‌شوند.** ترتیب شروع با `dependsOn`. `StatefulSet` با نام‌های پایدار (`db-0`، `db-1`)، شروع مرتب، یک volume برای هر replica و DNS برای هر pod. `DaemonSet` با یک pod روی هر نود (یا نودهای انتخاب‌شده) و rolling update. `Job` با completions، parallelism، backoffLimit و deadline. `CronJob` با زمان‌بندی cron و time zone و concurrencyPolicy |
| **Scheduler** | فیلتر بر اساس Ready/cordon، `nodeSelector`، CPU/RAM درخواستی، host port و محل volume. پخش replicaها روی نودهای مختلف |
| **Self-healing** | restart با backoff نمایی برای هر کانتینر، health check از نوع HTTP و TCP، جابه‌جایی podها وقتی نودی از دست می‌رود |
| **Networking** | subnet جداگانه برای هر نود، overlay رمزنگاری‌شده با **WireGuard** (یا host-gw)، ارتباط مستقیم pod به pod |
| **Services** | **TCP و UDP**، مسیریابی در سطح کرنل با iptables DNAT روی هر نود (IP مبدأ حفظ می‌شود؛ وقتی overlay خاموش است پراکسی userspace)، سرویس‌های `headless` (DNS مستقیم به IP podها) و `internal` (فقط داخل کلاستر)، load balancer روی سرورها |
| **NetworkPolicy** | قوانین ingress و egress بر اساس label podها، namespace، بازه‌ی IP و پورت، که با iptables روی هر نود اعمال می‌شوند |
| **Ingress** | مسیریابی HTTP/HTTPS بر اساس hostname (شامل `*.example.com`) و path روی پورت‌های 80/443، **گواهی خودکار Let's Encrypt** (`acme: true`، مشترک بین همه‌ی سرورها)، یا TLS از یک Secret نوع `tls` یا گواهی self-signed، و redirect از HTTP به HTTPS |
| **DNS** | `web`، `web.prod`، `web.default.svc.nube`، `db-0.db` و `POD.default.pod.nube` |
| **Storage** | `Volume` از نوع `local` با **حجم اجباری** (هر volume یک فایل‌سیستم ext4 مستقل است و بیشتر از اندازه‌اش نمی‌شود نوشت)، `nfs` (اشتراکی، یا `ReadWriteOnce` با حجم اجباری که همراه pod به هر نودی می‌رود) و `hostPath`. گزارش مصرف واقعی هر volume |
| **Snapshot و backup** | `VolumeSnapshot` که روی سرورهای control plane ذخیره و بین آن‌ها تکثیر می‌شود، backup زمان‌بندی‌شده با نگه‌داشتن تعداد مشخص، ساخت volume جدید از snapshot، جابه‌جایی volume به نود دیگر با `nube volume move`، و بازیابی خودکار روی نود سالم وقتی نود volume از دست برود |
| **توسعه‌پذیری** | `CustomResourceDefinition` با اعتبارسنجی schema و مقدار پیش‌فرض، شیءهای سفارشی با status، watch و garbage collection، کلاینت Go ‏(`pkg/client`) و یک operator نمونه |
| **امنیت** | **API روی HTTPS** با CA اختصاصی کلاستر (کلاینت‌ها hash آن را pin می‌کنند) یا گواهی Let's Encrypt یا گواهی خودتان برای داشبورد، **mutual TLS** بین سرور و agentها، **توکن‌های چندگانه با نقش** (`admin`، `edit`، `view`، `node`) و محدودسازی به namespace، و ماسک‌شدن مقدار secretها |
| **High availability** | ۳ (یا ۵) سرور با **Raft** که در هر تغییر فقط همان بخش تغییرکرده را تکثیر می‌کند. در صورت از کار افتادن leader، ظرف چند ثانیه leader جدید انتخاب می‌شود. agentها خودکار به سرور سالم سوییچ می‌کنند |
| **Runtime** | Docker یا Podman (از طریق API سازگار با Docker)، برای هر نود جداگانه |
| **Agentهای هوش مصنوعی** | `kind: Agent`: مدل (Claude یا هر سرور سازگار با OpenAI مثل vLLM و Ollama)، دستورالعمل و ابزارها (سرویس‌های HTTP، سرورهای MCP، کانتینر sandbox برای دستورهای shell، agentهای دیگر و خود Nube). هر کار یک `AgentRun` در pod خودش است با trace کامل، سقف token و step، timeout، صف برای هر agent، حافظه‌ی session، زمان‌بندی cron و تأیید انسان برای ابزارهای پرخطر |
| **مناسب برای agentها** | **MCP server** داخلی (`nube mcp`)، نقش `agent` که تغییرات مخربش منتظر **تأیید انسان** می‌ماند، **audit log**، `nube diagnose` (مشکل، شاهد و راه‌حل)، `--dry-run` و `nube diff`، `nube explain`، خطاهای JSON با کد ثابت و exit code |
| **Observability** | داشبورد زنده، رویدادها، لاگ زنده‌ی هر کانتینر (`logs -c`)، مصرف CPU، RAM و دیسک، و **پیشنهاد مقدار درخواست CPU/RAM** بر اساس مصرف واقعی |

## چیزهایی که Kubernetes به add-on واگذار کرده

Kubernetes خیلی پخته‌تر است و اکوسیستم بزرگی دارد. اما خیلی از نیازهای روزمره داخل خود Kubernetes نیست و برای هرکدام
باید یک پروژه‌ی جدا پیدا، نصب، تنظیم و به‌روز کرد. در Nube این‌ها بخشی از همان یک باینری هستند.

| مشکل روزمره | در Kubernetes | در Nube |
|---|---|---|
| ConfigMap یا Secret عوض می‌شود ولی podهای در حال اجرا تا restart بعدی مقدار قدیمی را دارند | نصب Reloader یا annotation دستی | داخلی: podها یکی‌یکی جایگزین می‌شوند و در history نوشته می‌شود `configmap/app changed` |
| podهای جدید یک rollout کرش می‌کنند یا image وجود ندارد | rollout فقط متوقف می‌شود؛ `progressDeadlineSeconds` فقط آن را failed علامت می‌زند و باید دستی `kubectl rollout undo` زد | داخلی: بعد از `progressDeadlineSeconds` نسخه‌ی قبلی خودکار و بدون قطعی برمی‌گردد و یک event دلیلش را می‌گوید |
| API باید منتظر دیتابیس بماند | ترتیبی وجود ندارد؛ معمولاً init container با حلقه‌ی انتظار می‌نویسند | `dependsOn: [db]`: pod صبر می‌کند تا سرویس `db` یک pod آماده داشته باشد |
| مقدار درخواست CPU/RAM چقدر باشد؟ | نصب Vertical Pod Autoscaler | `nube top workloads` و داشبورد درخواست‌ها را با مصرف واقعی مقایسه و مقدار پیشنهاد می‌کنند |
| گواهی HTTPS | نصب cert-manager | `acme: true` روی Ingress |
| backup، restore و انتقال داده‌ی volume به نود دیگر | نصب Velero و CSI snapshot controller | داخلی: snapshot، backup زمان‌بندی‌شده، `restoreFrom`، `nube volume move` و `autoRecover` |
| حجم volume محلی اعمال نمی‌شود | `hostPath` و local PV سهمیه ندارند | هر volume محلی فایل‌سیستم خودش را دارد و بیشتر از اندازه‌اش نمی‌شود نوشت |
| Ingress و load balancer روی سرور فیزیکی | نصب ingress controller و MetalLB | داخلی، روی همه‌ی سرورها |
| NetworkPolicy | فقط با CNI‌ای که پشتیبانی کند (Calico، Cilium و ...) | داخلی |
| اجرای control plane | etcd و چند جزء دیگر، و مدیریت گواهی‌ها | یک باینری؛ Raft و CA کلاستر داخلش هستند |
| اجرای agentهای هوش مصنوعی | یک فریم‌ورک داخل image خودتان، به‌علاوه‌ی صف، بودجه، trace و فرایند تأیید که خودتان بسازید | `kind: Agent`: run، trace، بودجه، sandbox، حافظه و تأیید داخلی هستند |
| مدیریت کلاستر به دست یک agent | یک MCP server شخص ثالث با تمام اختیارات kubeconfig | MCP server داخلی؛ نقش `agent` تغییرات مخرب را برای انسان نگه می‌دارد؛ همه‌ی تغییرات audit می‌شوند |

جاهایی که Kubernetes انتخاب بهتری است: کلاسترهای بزرگ، سازگاری با `kubectl`، Helm و operatorهای موجود، سرویس‌های
managed، و هر جا که یک پلتفرم استاندارد و آزموده لازم است.

## چه چیزهایی واقعاً تست شده
روی یک کلاستر Docker با ۳ سرور HA (با TLS) و نودهای Docker-in-Docker و Podman (با WireGuard)، و یک نصب تمیز روی Ubuntu 24.04:

- **Pod چندکانتینری:** init container صفحه را نوشت، nginx سرو کرد، و sidecar از طریق `localhost` به آن رسید.
- **StatefulSet:** `db-1` فقط بعد از Ready شدن `db-0` بالا آمد. هر replica volume خودش را داشت. `db-0.db` به pod درست resolve شد. rolling restart از بالاترین ordinal انجام شد.
- **Volume:** نوشتن ۱۰۰MB در یک volume با حجم ۶۴Mi با خطای «No space left on device» متوقف شد.
- **Job و CronJob:** Job دقیقاً ۳ completion را دوتا‌دوتا اجرا کرد. CronJob هر دقیقه اجرا شد و history limit رعایت شد.
- **UDP:** سرویس UDP هم داخل کلاستر و هم از طریق load balancer کار کرد.
- **Ingress:** مسیریابی با host و path، و HTTPS با گواهی همان Secret.
- **امنیت:** توکن view نتوانست تغییری بدهد، توکن edit محدود به `dev` نتوانست `default` را ببیند، توکن revoke‌شده فوراً از کار افتاد، و CA اشتباه رد شد.
- **HA:** با kill کردن leader، leader جدید انتخاب شد و درخواست‌ها روی HTTPS به آن فرستاده شد.
- **DaemonSet و Service:** دقیقاً یک pod روی هر نود اجرا شد. سرویس‌ها IP مبدأ را حفظ کردند و بار را بین نودها پخش کردند.
- **NetworkPolicy:** ترافیک روی هر دو نود مسدود و مجاز شد و با حذف policy دوباره باز شد.
- **CRD و operator:** operator نمونه شیءهای نوع `Website` را ساخت، scale کرد و بعد از حذف پاک کرد. schema شیءهای نامعتبر را رد کرد.
- **Snapshot:** snapshotها روی هر ۳ سرور تکثیر و دانلود شدند. یک volume به نود دیگر منتقل شد و یکی دیگر بعد از از دست رفتن نودش بازیابی شد، با داده‌هایشان.
- **NFS:** volume از نوع `ReadWriteOnce` حجمش را رعایت کرد و همراه pod به نود دیگر رفت.
- **Let's Encrypt:** با یک CA تستی ACME ‏(Pebble)، همه‌ی سرورها گواهی صادرشده را برای API و Ingress سرو کردند. بعد از توقف leader، leader جدید برای یک host تازه گواهی گرفت.
- **Podman:** روی یک نود Podman، pod چندکانتینری، DNS، سرویس‌ها، volume با حجم اجباری و snapshot کار کردند.
- **Agent:** یک run روی دو نود Podman (با یک جایگزین برای API مدل): pod مربوط به runtime از ابزارهای خود Nube استفاده کرد، دستور
  sandbox را تا تأیید نگه داشت، آن را در کانتینر busybox اجرا کرد، trace و حافظه‌ی session را ثبت کرد و پاک شد؛ دستور رد‌شده به‌شکل
  خطا به مدل رسید؛ پل MCP روی stdio ابزارهای `diagnose` و `run_agent` را سرو کرد. آداپتورهای Claude (SDK رسمی Go) و
  OpenAI-compatible، تأییدها، محدودیت token، صف و timeout با تست در برابر API‌های شبیه‌سازی‌شده پوشش داده شده‌اند.
- **Rollout هوشمند:** روی دو نود Podman، تغییر ConfigMap باعث rolling restart شد و سرویس مقدار جدید را برگرداند؛ کلاینتی با
  `dependsOn: [db]` تا آماده‌شدن db صبر کرد؛ rollout به imageای که وجود نداشت podهای قدیمی را نگه داشت و بعد از ۴۰ ثانیه خودکار rollback شد.

---

## Agentهای هوش مصنوعی

```yaml
kind: Agent
metadata: { name: ops-assistant }
spec:
  model: { name: claude-opus-5-5, apiKeySecret: anthropic }   # Secret با کلید ANTHROPIC_API_KEY
  instructions: You help operate this cluster. Diagnose before changing anything.
  tools:
    - nube: { role: view }                       # ابزارهای خود Nube: diagnose، لاگ، رویدادها ...
    - name: shell
      sandbox: { image: "alpine:3.20" }          # دستورها در یک کانتینر جدا اجرا می‌شوند
      requireApproval: true                      # هر فراخوانی را یک انسان تأیید می‌کند
    - agent: { name: researcher }                # واگذاری به agent دیگر (ابزار ask_researcher)
  memory: { enabled: true }
  limits: { maxSteps: 25, maxTokens: 400000, timeoutSeconds: 900, concurrency: 2 }
  schedules: [{ cron: "0 8 * * 1-5", input: "Summarize what needs attention today" }]
```

```bash
nube run ops-assistant "Why is web degraded?" --session alice --wait
nube trace RUN -w          # فراخوانی‌های مدل و ابزار، token‌ها و خروجی
nube approvals             # فراخوانی‌هایی که منتظر انسان‌اند؛ nube approve|reject ID
```

control plane هر run را در صف می‌گذارد (همزمانی برای هر agent)، یک pod با **runtime** (imageای که هر نود از باینری `nube` خودش
می‌سازد) و یک کانتینر برای هر ابزار sandbox می‌سازد، و به runtime توکنی می‌دهد که فقط برای همان run معتبر است و هیچ‌جا ذخیره
نمی‌شود. runtime حلقه‌ی agent را با SDK رسمی Anthropic برای Go (یا کلاینت سازگار با OpenAI) اجرا می‌کند، هر گام را گزارش می‌دهد،
برای تأیید می‌پرسد و با رسیدن به سقف step، token یا زمان متوقف می‌شود. نمونه‌ها در [`examples/agents`](examples/agents).

## مدیریت به دست agentهای هوش مصنوعی

```bash
nube token create claude --role agent -n staging     # تغییر مجاز است؛ تغییرات مخرب منتظر انسان می‌مانند
claude mcp add nube -- nube mcp                      # Claude Code (یا هر کلاینت MCP) ابزارهای Nube را می‌گیرد
```

ابزارهای MCP (`cluster_overview`، `diagnose`، `get_logs`، `explain`، `apply_manifest` با `dry_run`، `scale`، `rollout`،
`delete_resource`، `run_agent` و ...) فراخوانی عادی API با توکن خود فراخوان هستند: نقش، namespace، تأیید و audit log
همه اعمال می‌شوند. راهنما: [AGENTS.md](AGENTS.md).

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
برای اجرای podهای این نود روی Podman به‌جای Docker، گزینه‌ی `--runtime podman` را اضافه کنید.
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

### ۵) Let's Encrypt
```bash
sudo bash install.sh server ... --acme-email you@example.com [--acme-domain nube.example.com]
```
از این به بعد hostهای Ingress که `acme: true` دارند خودکار گواهی می‌گیرند. DNS باید به سرورها اشاره کند و پورت 80 یا 443
از اینترنت در دسترس باشد. با `--acme-domain` داشبورد و API هم گواهی معتبر می‌گیرند.
فقط leader با Let's Encrypt صحبت می‌کند. گواهی‌ها و پاسخ challengeها تکثیر می‌شوند، پس هر سروری می‌تواند جواب بدهد.
برای استفاده از گواهی خودتان روی API، سرور را با `--tls-cert` و `--tls-key` اجرا کنید.

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
nube get nodes|deploy|sts|ds|jobs|cronjobs|pods|svc|ing|netpol|cm|secrets|volumes|snapshots|crd [-n NS | -A] [-o wide|yaml] [-w]
nube get websites                  # هر نوع سفارشی، با نام جمع یا نام کوتاه
nube describe pod NAME
nube logs POD [-c CONTAINER] [-f]
nube scale deploy|sts NAME --replicas 5
nube set image deploy NAME nginx:1.27 [-c CONTAINER]
nube rollout status|history|undo|restart deploy|sts NAME
nube trigger cronjob NAME          # اجرای فوری
nube snapshot VOLUME [--name NAME] # گرفتن snapshot
nube snapshot download NAME FILE   # ذخیره به‌صورت فایل tar
nube volume move VOLUME --to NODE  # انتقال volume از نوع local (و pod آن) به نود دیگر
nube suspend|resume cronjob NAME
nube token create ci --role edit -n prod       # فقط یک بار نشان داده می‌شود
nube token list | nube token delete ci
nube whoami
nube top nodes|pods|workloads      # workloads: درخواست‌ها در برابر مقدار پیشنهادی
nube diagnose [KIND NAME]          # چه چیزی خراب است، شاهد و راه‌حل
nube diff -f app.yaml              # apply چه چیزی را عوض می‌کند، فیلد به فیلد
nube explain [KIND]                # فیلدهای یک نوع و یک نمونه
nube run AGENT "task" --wait | nube trace RUN -w | nube approvals | nube approve ID | nube audit
nube mcp                           # MCP server روی stdio
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
kind: Deployment            # یا StatefulSet / DaemonSet / Job / Pod
metadata: { name: app, namespace: default }
spec:
  replicas: 3
  progressDeadlineSeconds: 600   # اگر podهای جدید بعد از این مدت هنوز خراب باشند → rollback خودکار
  # autoRollback: false          # فقط گزارش بده
  template:
    dependsOn: [db]              # فقط وقتی سرویس db یک pod آماده دارد schedule شود
    # reloadOnConfigChange: false   # با تغییر ConfigMap/Secret، podها را عوض نکن
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
spec:
  type: local
  size: 10Gi                                  # حجم اجباری
  backup: { schedule: "0 3 * * *", keep: 7 }  # snapshot شبانه
  autoRecover: true                           # اگر نود از دست برود، روی نود دیگر بازیابی شود
  # restoreFrom: data-snap                    # شروع از یک VolumeSnapshot
---
kind: NetworkPolicy
metadata: { name: db-access }
spec:
  podSelector: { matchLabels: { app: db } }
  ingress:
    - from: [{ podSelector: { matchLabels: { role: api } } }]
      ports: [{ port: 5432 }]
---
kind: Ingress
metadata: { name: web }
spec:
  tls: [{ hosts: [app.example.com], acme: true }]
  rules:
    - host: app.example.com
      paths: [{ path: /, backend: { service: app, port: 8080 } }]
```

وقتی از `containers:` استفاده می‌کنید، `image`، `command`، `ports`، `volumes` و بقیه را داخل هر کانتینر بنویسید. همین فیلدها
در سطح template مخصوص حالت تک‌کانتینری هستند و ترکیب این دو رد می‌شود.

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
| `agents/` | دستیار عملیات (ابزارهای Nube، shell با تأیید، حافظه، زمان‌بندی)، تیم دو agentی، agent با مدل محلی |
| `operator/` | یک CRD به نام `Website` و یک operator (با Go و `pkg/client`) که از آن Deployment، Service و Ingress می‌سازد |

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
- **HA:** هر تغییر state یک entry در لاگ Raft است که فقط شیءهای تغییرکرده را دارد (patch). heartbeat و آمار مصرف فقط در حافظه‌ی
  leader می‌مانند. فقط leader کنترلرها را اجرا می‌کند. کد: `internal/store/raft.go` و `patch.go`.
- **شبکه:** هر نود یک شبکه‌ی bridge ‏(`nube0`) با یک /24 از `10.244.0.0/16` دارد. WireGuard ‏(`nube-wg`) subnetها را بدون NAT بین
  نودها مسیریابی می‌کند. سرویس‌ها قوانین iptables DNAT هستند و NetworkPolicyها زنجیره‌های iptables برای هر pod. DNS روی
  gateway شبکه (`10.244.N.1`) گوش می‌دهد.
- **Snapshot:** agent از volume یک tar می‌سازد و روی سرورها آپلود می‌کند و سرورها آن را بین خودشان تکثیر می‌کنند. بازیابی آن را در
  یک volume تازه روی نود مقصد باز می‌کند. انتقال و بازیابی خودکار هم همان snapshot و restore است.
- **Rollout آگاه از تنظیمات:** revision هر workload از template و یک digest کلیددار (HMAC) از ConfigMapها و Secretهایی که
  می‌خواند ساخته می‌شود؛ پس تغییر داده یک rolling update عادی شروع می‌کند و history چیزی از مقدار secretها لو نمی‌دهد.
- **Rollback خودکار:** اگر وقتی `progressDeadlineSeconds` تمام می‌شود podهای revision جدید خراب باشند (crash loop، خطای pull،
  health check ناموفق، ConfigMap ناموجود)، کنترلر template قبلی را برمی‌گرداند. podهایی که فقط منتظرند (schedule، pull، `dependsOn`)
  باعث rollback نمی‌شوند و خود rollback هیچ‌وقت rollback نمی‌شود. کد: `internal/server/rollout.go`.
- **پیشنهاد منابع:** leader هر دقیقه بیشترین مصرف هر pod از هر workload را ثبت می‌کند (یک روز، در حافظه). CPU با صدک ۹۵ به‌علاوه‌ی ۱۵٪
  و RAM با بیشینه به‌علاوه‌ی ۲۰٪ پیشنهاد می‌شود، چون RAM سقف کانتینر هم هست.
- **CRD:** یک CRD نوع جدیدی به API اضافه می‌کند، با زیرمجموعه‌ای از JSON schema برای اعتبارسنجی و مقدار پیش‌فرض. operatorها از
  `watch` استفاده می‌کنند و `status` را می‌نویسند. شیءهایی که label ‏`nube/owned-by=Kind/name` دارند همراه صاحبشان حذف می‌شوند.

## محدودیت‌هایی که هنوز هست

- manifestهای Kubernetes و `kubectl` پشتیبانی نمی‌شوند. Nube فرمت manifest و CLI ساده‌تر خودش را دارد.
- volume از نوع `local` روی یک نود است. انتقال یا بازیابی آن آخرین snapshot را برمی‌گرداند، پس اگر نود از کار بیفتد نوشته‌های بعد از
  آن snapshot از دست می‌روند. برای داده‌ای که باید بدون restore از خرابی نود جان سالم به در ببرد از `nfs` با `ReadWriteOnce` استفاده کنید.
- snapshotها روی سرورهای control plane ذخیره می‌شوند، پس دیسک سرورها حجم کل آن‌ها را محدود می‌کند.
- volumeهای اشتراکی `nfs` ‏(`ReadWriteMany`) حجم اجباری ندارند و این با سرور NFS است.
- هر سرور کل state را در حافظه نگه می‌دارد. این برای ده‌ها نود و چند هزار pod مناسب است.
- فقط Docker و Podman پشتیبانی می‌شوند (containerd/CRI نه).
- sandbox مربوط به agentها هم ایزوله‌سازی بیشتری از Docker یا Podman ندارد؛ برای ابزارهایی که دستور اجرا می‌کنند از `requireApproval` استفاده کنید.
- trace و حافظه‌ی agentها در state تکثیرشده نگه داشته می‌شوند؛ هر گام حداکثر ۴KB و برای هر agent صد run آخر نگه داشته می‌شود.
- workflowها با خود مدل هدایت می‌شوند (ابزار و واگذاری)؛ زبان تعریف گراف/workflow وجود ندارد.
- ایزوله‌سازی podها بیشتر از آنچه Docker یا Podman می‌دهند نیست (user namespace، پروفایل seccomp سفارشی یا runtime ایزوله ندارد).

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
| Ingress خطای `acme:` نشان می‌دهد | DNS باید به سرورها اشاره کند و پورت 80 یا 443 از اینترنت در دسترس باشد |
| `nube apply` می‌گوید فیلدها را داخل هر کانتینر بگذارید | هم `containers:` دارید و هم فیلدهایی مثل `volumes:` در سطح بالا؛ آن‌ها را داخل کانتینر ببرید |
