# Chaos scenariji

Realne situacije koje sistem mora da preživi, svedene na ponovljive korake.
Svi su izvedeni protiv docker stack-a (`make stack-up`) sa lokalnim agentsim
fleet-om. Chaos ide kroz agente (chaos-ui Chaos tab ili agentsim control API na :7780), nikad direktno u bazu —
agent laže ili ćuti preko pravog gRPC transporta.

chaos-ui uopšte nema pristup bazi: topologiju čita i desired state piše preko
apiserver control plane-a (`CONTROL_PLANE_URL`, podrazumevano :7080), pa UI i
CLI prolaze kroz isti project sloj. Operator chaos (cordon/drain/delete replica)
ide na isti control plane, agent chaos na agentsim.

## Stanje hosta: dve kolone, dva vlasnika

`hosts.host_healthy` (bool) piše samo heartbeat/watchdog: heartbeat → `true`,
30s tišine → `false`. `hosts.status` piše samo operator: `open | cordoned |
draining`. Slobodno smeštanje ide samo na `host_healthy AND status='open'`;
volume-pinovana replika sme nazad na svoj host i kad je `cordoned`/`draining`
(disk je tu i nigde drugde). Heartbeat nikad ne dira `status`, smrt hosta nikad
ne briše drain. (Do 2026-09-18 sve je bilo u jednoj koloni
`ready|notready|draining|cordoned` — stara merenja dole koriste te reči.)

## Pragovi (internal/engine)

| Konstanta | Vrednost | Značenje |
|---|---|---|
| `watchdogInterval` | 5s | koliko često watchdog proverava staleness i settle-uje drainove |
| `hostUnhealthyAfter` | 30s | tišina → `host_healthy=false`, van scheduling-a (reverzibilno) |
| `hostDeadAfter` | 2min | tišina → replike se oslobađaju (jednosmerno) |
| `drainStalledAfter` | 10min | drain u letu duže od ovog → WARN u logu svaki sweep; bez akcije |
| `volumeLeaseTTL` | 90s | bez healthy observacije → lease ističe, failover sme |
| startup grace | = `hostDeadAfter` | posle boot-a engine-a nema presuda smrti dok ne protekne pun prozor |

## Postavka

```bash
make stack-up      # postgres + engine + apiserver + agentsim + chaos-ui (localhost:3000)
make build
# u praznom folderu:
./build/conductor init -n chaos-demo
./build/conductor add --service --name web --image nginx:alpine
./build/conductor up -s web                    # config.toml: 2 replike, us-east-1
```

Agentsim je deo stack-a (jedan sim-agent po hostu, control API na :7780).
Chaos ide kroz UI (Chaos tab) ili direktno na control API; ID-jeve daje
`curl localhost:7780/agents` (host + replika + faza + chaos mod). Akcija:
`curl -XPOST localhost:7780/chaos -d '{"action":"host_kill","host":"<id>"}'`
(akcije: `host_kill|host_recover`, `replica_crash|replica_crashloop|replica_stall_health|replica_heal`,
`volume_stall_resize|volume_heal` sa `"volume":"<id>"`).

## 1. Mrežni blip (< 2min) — ništa se ne pomera

Zahtev: kratka smetnja ne sme da scrambluje workload.

- chaos `host_kill` <host> → host ćuti
- ~30s: host `unhealthy`, van scheduling-a; **replike ostaju vezane i active**
- chaos `host_recover` <host> pre 2min
- prvi heartbeat vraća `healthy`; nijedna replika nije mrdnula

Izmereno kroz chaos-ui (2026-09-11): kill → notready 30s → recover → ready 1s;
replika netaknuta.

Izmereno: kill 11:54:42 → notready 11:55:14 (32s) → recover 11:55:27 → ready
11:55:32 (5s). Replike netaknute ceo period.

## 2. Smrt hosta (> 2min) — re-place na preživele

Zahtev: mrtav host gubi replike; one se automatski re-place-uju na druge
hostove istog regiona; host koji kasnije oživi vraća se prazan u pool.

- chaos `host_kill` <host>, ne oporavljaj
- ~30s: `unhealthy` (kao gore)
- ~2min: watchdog `MarkHostDown` — replike hostless `replacing`, sledeći tick
  placer ih dodeli drugom hostu, agent ih podigne kroz start → health → active
- `recover-host` bilo kad posle: host se vraća `healthy`, prazan; orphan
  kontejnere agent sam ugasi na prvom full snapshotu (nisu više u njegovoj listi)

Izmereno kroz chaos-ui (2026-09-11): notready 34s → replacing na 2min04s → active na
drugom hostu 3s kasnije; recover vraća host `ready` prazan za 1s.

Izmereno: kill 11:55:47 → notready 11:56:19 (32s) → replika oslobođena
11:57:49 (2min02s) → active+healthy na novom hostu 11:57:53 (4s posle presude).

## 3. Restart control plane-a — startup grace, bez masakra

Zahtev: pad/redeploy engine-a duži od staleness prozora ne sme po boot-u da
proglasi ceo fleet mrtvim (heartbeat-i su stali zato što je gateway bio down,
ne zato što su hostovi mrtvi).

- `docker compose stop engine`, sačekaj 2-3min (svi heartbeat-i staju)
- `docker compose start engine`
- agenti se rekonektuju (gRPC backoff ume da doda i ~30s posle dužeg pada);
  sweep sme da demote-uje u `unhealthy` (reverzibilno), ali presuda smrti ne
  pada dok uptime engine-a ne pređe `hostDeadAfter` — do tada su se svi živi javili
- očekivano: nula oslobođenih replika, fleet se vrati `healthy` bez ijednog restarta

Izmereno (apiserver kao gateway, 2026-09-11): `docker compose stop apiserver` 153s →
watchdog demote 5 hostova na +35s, nula presuda smrti → agenti nazad ~64s posle
starta (gRPC backoff) → svi `ready` na +217s, replike netaknute.

Izmereno: outage 11:58:29→12:00:59 (2.5min) → na +30s sweep demote-ovao svih 6
hostova (agenti još u backoff-u) → agenti nazad +39s → heartbeat vratio `ready`.
`hostless=0`, `active=2` tokom celog ciklusa — nijedna replika ni restartovana
ni pomerena.

## 4. Crash-loop replike — restart budžet

Zahtev: kontejner koji stalno umire ne sme da vrti sistem u krug zauvek.

- chaos `replica_crashloop <replica>` → restart_count raste svaki tick
- kad pređe `restart_max`, reconciler-ovo `crashLooping` pravilo obara **ceo
  deployment** u `failed` i `deploymentFrozen` ga zamrzava: nema re-place-a,
  nema zamene, replika ostaje da se vrti dok operator ne uradi redeploy/rollback
- to je namerno: iscrpljen restart budžet je signal za čoveka, ne za automatiku
- chaos `replica_heal <replica>` posle toga vraća kontejner u normalan hod, ali
  deployment ostaje `failed` — jedini izlaz je `conductor up`/`rollback`

Izmereno (kroz chaos-ui): restart_max=5 → deployment `failed` za ~6s; replika
ostala `starting` sa restart_count u stotinama dok nije stigao sledeći deploy.

## 5. Zaglavljen health check — progress deadline

Zahtev: deploy koji nikad ne postane healthy ne sme da visi večno.

- chaos `replica_stall_health` <replica> → kontejner se podigne, probe nikad ne prođu
- replika stoji u `health_check`; progress-deadline putanja je obara i
  rollout se završava kao failed umesto da visi

Izmereno kroz chaos-ui (progress_deadline=60): kanarinac stall → deployment `failed`
za 56s, served revision ostao na staroj verziji. Napomena: kontejner postoji na agentu
tek ~3s posle deploya, stall pre toga vraća 404 (`no agent runs replica`).

## 6. Zombi agent + stateful lease — single writer

Zahtev: particionisan-ali-živ agent ne sme da drži volume zauvek niti da
vaskrsne otpisanu repliku.

- stateful servis (`conductor add --database ...` + volume), replika drži lease
- `kill-host` njenog hosta; posle 2min replika ide u `replacing`
- agent nastavi da šalje healthy za nju (zombi): observacije padaju na SQL
  guard (`replacing` je orchestrator-owned) i — bitno — **ne obnavljaju lease**
- lease istekne 90s od poslednje observacije koja je stvarno prošla; tek tad
  zamenska replika sme `AcquireVolumeLease` → single writer očuvan, failover
  nije blokiran

Izmereno kroz chaos-ui (2026-09-11): replacing na +125s, replika ostaje hostless (pin na
volume hosta), lease istekao ~+90s; recover → ista replika nazad na isti host active za 4s
i lease ponovo uzet.

## Regularni scenariji (bez chaosa)

### 7. Scale up / down

- `conductor scale us-east-1=4 -s web` → placer dodaje replike uz anti-affinity
  (širi po hostovima pre nego što duplira)
- `conductor scale us-east-1=2 -s web` → višak ide u `draining`, reap posle
  `drain_seconds`; traffic pointer se NE dira (scale-down nije rollout)

Izmereno: 2→4 active+healthy za **8s** (spread na 3 hosta); 4→2: draining na
+9s, reaped na +12s (drain window 10s). Kroz chaos-ui (2026-09-11): 2→4 za 5s, 4→2 za 11s.

### 8. Novi deploy — blue/green

- izmeni spec/image pa `conductor up -s web` → v2 replike se dižu paralelno sa
  v1; kad su sve healthy, traffic switch je atomski batch (SetServedRevision +
  drain starih u istoj transakciji); v1 replike se drain-uju pa reap-uju
- pad v2 (crash/stall pre nego što postane healthy) → progress deadline obara
  rollout kao failed, v1 ostaje da služi

Izmereno: `up` v2 → v2 current (2 active) i v1 reaped za **12s**. Kroz chaos-ui
(2026-09-11): deploy forma → v2 served i v1 reaped za 21-22s (drain 10s).

### 9. Stateful servis — volume, lease, recreate

- `conductor add --database --engine postgres --name pg` +
  `conductor volume add --mount /var/lib/postgresql/data --size 2 -s pg` +
  `conductor up -s pg -f pg-config.toml` (num_replicas=1 — stateful je single
  instance)
- placer prvo smesti volume (3D bin-pack: cpu/mem/disk), replika prati volume
  host; lease se uzima u istoj transakciji kao dodela hosta
- redeploy (`up` opet) je recreate, ne blue/green: stara replika ode, nova
  preuzima lease — nikad dva pisca

Izmereno: deploy→active+lease za **13s** (replika i volume na istom hostu);
recreate v1→v2 sa lease handover-om za **19s**, ceo period tačno 1 živa replika.
2026-09-11: deploy→active+lease 6s; recreate: stara draining → reaped +13s, nova
scheduling +16s, active + lease +18s.

### 11. Volume resize — grow-only, engine je gate za prostor

Zahtev: `conductor volume update --size N` mora da poraste disk uživo bez
restarta replike (kao Railway live resize); shrink ne postoji; zahtev koji host
ne može da primi ne sme da završi u `failed` nego čeka dok se prostor ne pojavi.

Model: CLI menja samo `desired_size_bytes`. Engine svaki tick gleda drift
(`desired > observed`) i, ako ledger hosta ima mesta (grow sme da potroši ceo
`DiskReserve`, nova plasiranja ne smeju), flipuje status `attached → resizing`.
Tek taj status otključava novu veličinu na downlinku (`volumeTargetSize`), pa
agent nikad ne raste disk koji host ne drži. Agent javlja `VolumeObservation`
sa stvarnom veličinom; kad `observed >= desired` engine vraća `resizing →
attached`. Oba flipa su po jedan red u sopstvenoj tx sa SQL predikatom
(`MarkVolumeResizing`/`MarkVolumeAttached`), izgubljena trka = drop, sledeći
tick odlučuje ponovo.

- postavka kao u 9 (stateful `pg`, `volume add --size 2`, `up`)
- `conductor volume update --mount /var/lib/postgresql/data --size 4 -s pg`
  → CLI kaže "host has room"; engine `resizing` na sledećem ticku; agent
  naraste disk u jednom ticku i javi; engine `attached`
- `--size 100` na hostu sa 80GB (budžet 64GiB) → CLI kaže "host is short
  36GiB … waits"; `volume list` pokazuje `SIZE 100GiB / ON DISK 4GiB /
  attached (grow waiting for host space)`; ništa se ne dešava koliko god tickova
- povlačenje: `--size 4` (= on disk) → "pending grow withdrawn"; pod on-disk
  veličinu CLI odbija (`grow-only`)
- prostor se pojavi (drugi volume ode sa hosta, ili operator doda disk:
  `update hosts set disk_bytes=…`) → engine sam odobri, bez akcije operatera

Izmereno (2026-09-14, agentsim tick 1s, reconcile 2s): update 10:17:05 →
`resizing` 10:17:06 → agent 4GiB 10:17:06 → `attached` 10:17:07 (**2s**).
Čekanje na prostor: 3 ticka bez promene, status label vidljiv u `volume list`.
Host 80→200GiB 10:17:55 → `resizing` +2s → `attached` na 100GiB +5s. Replika
`active|healthy|restart_count=0` ceo period, lease netaknut.

Chaos `volume_stall_resize <volume>`: engine odobri (`resizing`), agent nikad ne
javi novu veličinu → volume stoji `resizing`, `ON DISK` zaostaje; `volume_heal`
→ agent naraste i engine settle-uje za 2s. Nema timeout-a ni `failed`: stalled
resize je vidljiv drift za čoveka, ne presuda za automatiku (isti stav kao 4).

Dok je volume `resizing`, `update` je odbijen — downlink već šalje `desired`,
pa bi svaka promena preskočila disk gate; jedan resize u letu, čeka se
`attached`. Resize je za sad CLI-only — chaos-ui ne prikazuje volumene.

### 10. Operator akcije

- `cordon` (UI → apiserver): host ostaje da služi postojeće, ne dobija novo;
  primenjuje se samo na `open` (409 za `draining` — cordon bi izbrisao drain)
- `drain` (UI → apiserver): graciozna evakuacija stateless replika, host na
  kraju sam završi u `cordoned` — detalji i merenja u 12. Uvek prolazi (nema
  `force`, 409 samo ako već draina)
- `uncordon` (samo API, `POST /v1/hosts/{id}/uncordon`): vraća i `cordoned` i
  `draining` host u `open` i briše `drain_started_at` — tako se drain otkazuje
- `conductor rollback`: vrati prethodnu verziju deploymenta

### 12. Drain hosta — graciozna evakuacija

Zahtev: operator kaže "skini sve sa ovog hosta" i kapacitet servisa nikad ne
sme da padne ispod desired. Stateful replike (volume) se **ne sele** — disk je
lokalan; ostaju na hostu i operator ih migrira ručno kasnije.

Model: nema novog pravila ni Intent-a. `POST /v1/hosts/{id}/drain` upiše
`status='draining'`, `drain_started_at=now()`. Snapshot svakoj replici nosi
`host_draining` (LEFT JOIN hosts), a engine to sužava na stateless replike
(volume-pinovana ne odlazi sa hostom). Dve izmene u rolling kaskadi:

- `healthyTargets` ne broji repliku na draining hostu → `rollingRampUp` sam
  pravi surge (zamene se kreiraju dok stare još služe)
- `rollingScaleDown` sortira draining-host replike prve, pa newest-first →
  kad je zamena healthy, višak koji odlazi su tačno stare

`notAllHealthy` između njih gleda sirovo `Healthy`, ne kapacitet — inače bi
zdrava replika na draining hostu držala grupu zauvek pre nego što zamena
uopšte nastane. Kraj draina odlučuje watchdog sweep: `CompleteDrainedHosts`
prebaci `draining → cordoned` kad na hostu nema živih stateless replika
(`phase NOT IN (reaped, failed)` — čeka **reap**, ne drain, jer drained
replika još služi do isteka prozora). Placer ledger sadrži sve `host_healthy`
hostove, `status='open'` filtrira samo `pick` (slobodno smeštanje); DB pojas
`ReserveReplicaOnHost` isto: `host_healthy AND (status='open' OR volume_id
IS NOT NULL)`.

Postavka (svi podscenariji; 3 web replike u us-east-1 da anti-affinity stavi
po jednu na svaki od 3 hosta):

```bash
make stack-up && make build
mkdir demo && cd demo
../build/conductor init -n chaos-demo
../build/conductor add --service --name web --image nginx:alpine
printf '[deploy]\nnum_replicas = 3\nregion = "us-east-1"\ndrain_seconds = 10\ncpu = "200m"\nmemory = "128Mi"\n' > config.toml
../build/conductor up -s web
curl -s localhost:7080/v1/hosts | jq -r '.[] | "\(.hostname) \(.id) healthy=\(.host_healthy) \(.status) repl=\(.replicas_on_host)"'
```

`GET /v1/hosts` (i topology `hosts`) vraća `host_healthy`, `status`,
`drain_started_at`, `replicas_on_host` — drain se prati odatle ili iz
chaos-ui Hosts panela (dva badge-a: health + status, "draining since …",
broj replika).

#### 12a. Drain hosta sa stateless replikom — surge pa scale-down

- `curl -XPOST localhost:7080/v1/hosts/<ue1-medium-1>/drain`
- sledeći tick: `rollingRampUp → create` (jedna zamena; kanarinac ako na
  draining hostu stoje **sve** replike grupe, inače ceo deficit odjednom)
- zamena dobije open host, healthy → `rollingScaleDown → drain` stare
- stara `draining` još služi `drain_seconds`, pa `reapDrained → destroy`
- sweep ≤5s posle: `watchdog -> drain complete, host cordoned`; host ostaje
  `healthy`, `drain_started_at` se briše

Izmereno (2026-09-18, reconcile 2s, drain_seconds 10): drain 10:15:08 →
create +1s → zamena `active+healthy` +4s → stara `draining` +5s → `reaped`
+15s → host `cordoned` +17s. Broj healthy web replika ceo period ≥ 3.

#### 12b. Drain + smrt hosta sa stateful i stateless replikama (reboot)

Postavka dodatno: `add --database --engine postgres --name pg`, `volume add
--mount /var/lib/postgresql/data --size 2 -s pg`, `up -s pg -f pg-config.toml`
(num_replicas=1, us-east-1). pg je sleteo na `ue1-small-1` uz 2 web replike.

- `drain <ue1-small-1>` i odmah chaos `host_kill <ue1-small-1>` (agent zaćuti):
  `curl -XPOST localhost:7780/chaos -d '{"action":"host_kill","host":"<id>"}'`
- web: 2 zamene odjednom (postoji healthy web na drugom hostu, kanarinac nije
  potreban) → healthy → stare 2 `draining` → reap → **sweep cordonira host dok
  je mrtav**: kraj draina se čita iz redova replika, ne iz heartbeat-a
- pg ostaje vezan (stateful se ne seli); +30s host `unhealthy`; +2min
  `MarkHostDown` oslobodi pg → `replacing`, hostless, pinovan na volume čiji
  je host van ledgera → čeka (assign_host se odbacuje svaki tick)
- `status` posle smrti ostaje `cordoned` (MarkHostDown dira samo `host_healthy`)
- chaos `host_recover` → heartbeat vrati `healthy`, status i dalje `cordoned`;
  pinovana grana placera ignoriše status → pg nazad na **isti** host, lease
  ponovo uzet, active

Izmereno (2026-09-18): drain+kill 10:17:22 → 2× create +1s → zamene active +6s
→ stare draining +6s → reaped +15s → `cordoned` +18s → `unhealthy` +33s →
MarkHostDown +2min03s (pg `replacing`, hostless) → recover 10:20:14 → healthy
+1s → pg `health_check` na istom hostu +2s → `active` +3s. web ceo period 3
healthy.

#### 12c. Drain hosta na kome je samo stateful

- host sa samo pg (posle 12b): `uncordon` pa `drain`
- nema šta da se seli → prvi sweep: `cordoned`, pg netaknut, `active`

Izmereno: drain 10:20:32 → `cordoned` 10:20:36 (jedan sweep). Ovo je i
signal operatoru: host je "prazan koliko može automatski", ostatak je ručno.

#### 12d. Otkazivanje draina / uncordon

- `POST /v1/hosts/{id}/uncordon` na `draining` ili `cordoned` → 200, `status=open`,
  `drain_started_at=null`; host odmah ponovo prima placement
- `cordon` na `draining` → 409 (ne brisati drain nehotice); `drain` na
  `cordoned` → 200 (cordon se "pojačava" u evakuaciju)

Izmereno: uncordon 10:16:36 → `open`, `drain_started_at` null istog sekunda.

#### 12e. Zaglavljen drain — WARN, bez akcije

Kad zamena nema gde (region bez open hosta sa kapacitetom, ili zamena nikad
ne postane healthy), drain stoji: stara replika služi, host `draining`.

- `cordon` sve ostale hostove u regionu koji imaju mesta; `drain` host sa replikom
- `rollingRampUp → create`, zamena `pending` hostless; `anyHostlessReplicas`
  emituje `assign_host` svaki tick, placer ga odbacuje (nema open hosta)
- posle `drainStalledAfter` (10min): `watchdog -> drain stalled, still holding
  stateless replicas draining_for=…` WARN u svakom sweep-u; `GET /v1/hosts`
  pokazuje `drain_started_at` star 10+ min i `replicas_on_host > 0`
- ništa se ne dešava samo od sebe — čovek oslobodi kapacitet (`uncordon`,
  novi host) i drain se sam nastavi

Izmereno (2026-09-18): cordon `ue1-medium-1` + drain `ue1-large-1` 10:21:10 →
create +1s, zamena `pending` bez hosta → prvi WARN 10:31:15
(`draining_for=10m5s`), pa svakih 5s → uncordon `ue1-medium-1` 10:31:29 →
zamena `active` +3s → stara `draining` +4s → `reaped` +14s → `cordoned`
+17s. Stara replika služila celih 10min čekanja; broj healthy web ≥ 3.

Napomena: dok zamena čeka host, `anyHostlessReplicas` loguje `assign_host`
na INFO svaki tick (2s) — isto kao za pinovanu repliku na mrtvom hostu (12b);
pre-postojeći šum, nije deo drain-a.

## Kroz chaos-ui (localhost:3000)

Chaos tab pokriva sve akcije, po targetu:

- **Replika**: Crash (terminal `failed`), Crash loop (restart_count raste dok
  ne pukne budžet), Stall health checks (progress-deadline putanja), Heal
  (skini chaos), Delete row (orphan simulacija — jedini direktan DB upis)
- **Deployment**: Crash deployment / Stall rollout — fan-out iste agent-akcije
  na sve žive replike deploymenta
- **Host**: Kill host (agent zaćuti → `unhealthy` na ~30s → mrtav na 2min),
  Recover (prvi heartbeat vraća `healthy`), Cordon i Drain (operator desired
  state preko apiserver-a). Hosts panel nosi dva badge-a: health
  (`healthy|unhealthy`) i status (`cordoned|draining`, `open` se ne prikazuje),
  plus broj replika i "draining since …" dok drain traje

Sve agent-observable akcije UI prosleđuje agentsim control API-ju
(`AGENTSIM_URL`, u stacku `http://agentsim:7780`) — chaos putuje pravim
transportom (agent laže/ćuti preko gRPC-a), pa ga sledeći report ne može
pregaziti. Curl ekvivalent: `POST :7780/chaos` sa istim `action` poljem. Tranzicije gledaj na Topology tabu i u Logs
(`watchdog -> stale hosts out of scheduling`, `watchdog -> host down`,
`reconcile -> rule fired`).
